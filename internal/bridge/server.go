package bridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/theatlantadream/voicecoder-bridge/internal/agent"
	"github.com/theatlantadream/voicecoder-bridge/internal/clean"
	"github.com/theatlantadream/voicecoder-bridge/internal/whisper"
)

// chunkBytes is one second of 16 kHz mono Int16 PCM. Smaller chunks make
// partial transcripts arrive sooner; large-v3-turbo keeps up with that rate.
const chunkBytes = 32000

var blankRe = regexp.MustCompile(`\[.*?\]`)

type Server struct {
	transcriber whisper.Transcriber
	upgrader    websocket.Upgrader
	silenceRMS  float64
	silenceFor  time.Duration
	agent       agent.Agent
	repo        string
	origins     []string
	// progressEvery is how often a running agent reports that it is still
	// working when no partials have arrived yet. Tests shorten it.
	progressEvery time.Duration
}

type Option func(*Server)

func WithSilence(rms float64, d time.Duration) Option {
	return func(s *Server) {
		s.silenceRMS = rms
		s.silenceFor = d
	}
}

// WithAgent sends every cleaned request to a. Without it the bridge only
// transcribes and cleans.
func WithAgent(a agent.Agent) Option {
	return func(s *Server) {
		s.agent = a
	}
}

// WithRepo is the project folder used to write .voicecoder/latest.diff after
// an agent run so the editor can open the patch.
func WithRepo(repo string) Option {
	return func(s *Server) {
		s.repo = repo
	}
}

// WithAllowedOrigins limits which web pages may connect once an agent is
// set. Each entry is a scheme and host, such as "https://example.com".
func WithAllowedOrigins(origins []string) Option {
	return func(s *Server) {
		s.origins = origins
	}
}

func NewServer(t whisper.Transcriber, opts ...Option) *Server {
	s := &Server{
		transcriber: t,
		silenceRMS:  500,
		silenceFor:  1500 * time.Millisecond,

		progressEvery: 10 * time.Second,
	}
	s.upgrader = websocket.Upgrader{CheckOrigin: s.originAllowed}
	for _, o := range opts {
		o(s)
	}
	return s
}

// originAllowed lets every page in when no agent is set, as before. With an
// agent, text becomes a coding task, so only these may connect: tools that
// send no Origin header, a page served from the same host it connects to
// (the web app behind its own proxy), and pages on the allow list.
func (s *Server) originAllowed(r *http.Request) bool {
	if s.agent == nil {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	got, err := url.Parse(origin)
	if err != nil || got.Host == "" {
		return false
	}
	if strings.EqualFold(got.Host, r.Host) {
		return true
	}
	for _, allowed := range s.origins {
		want, err := url.Parse(strings.TrimSpace(allowed))
		if err != nil || want.Host == "" {
			continue
		}
		if !strings.EqualFold(got.Scheme, want.Scheme) || !hostMatches(want.Hostname(), got.Hostname()) {
			continue
		}
		host := strings.ToLower(want.Hostname())
		if host == "localhost" || host == "127.0.0.1" || got.Port() == want.Port() {
			return true
		}
	}
	return false
}

// hostMatches compares host names without case. A pattern that starts with
// "*." matches any subdomain, so "*.example.com" matches "app.example.com"
// but not "example.com".
func hostMatches(pattern, host string) bool {
	pattern, host = strings.ToLower(pattern), strings.ToLower(host)
	if suffix, ok := strings.CutPrefix(pattern, "*"); ok && strings.HasPrefix(suffix, ".") {
		return len(host) > len(suffix) && strings.HasSuffix(host, suffix)
	}
	return pattern == host
}

type control struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type outbound struct {
	Type string `json:"type"`
	Seq  int    `json:"seq"`
	Text string `json:"text,omitempty"`
	Diff string `json:"diff,omitempty"`
}

type session struct {
	mu            sync.Mutex
	listening     bool
	armed         bool // false after an explicit stop until the client sends start
	audio         []byte
	partials      map[int]string
	nextSeq       int
	pending       sync.WaitGroup
	runCtx        context.Context
	cancel        context.CancelFunc
	speechStarted bool
	lastSpeechAt  time.Time

	// The agent run has its own context so a new utterance never cancels it.
	baseCtx     context.Context
	agentCancel context.CancelFunc
	agentRun    int
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade: %v", err)
		return
	}

	defer conn.Close()

	sess := &session{partials: make(map[int]string), baseCtx: r.Context()}
	defer s.cancelAgent(sess)
	writeMu := &sync.Mutex{}
	send := func(msg outbound) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteJSON(msg)
	}

	for {
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		switch msgType {
		case websocket.BinaryMessage:
			s.handleAudio(r.Context(), sess, send, payload)
		case websocket.TextMessage:
			var c control
			if err := json.Unmarshal(payload, &c); err != nil {
				send(outbound{Type: "error", Text: "could not read that message, try again"})
				continue
			}
			s.handleControl(r.Context(), sess, send, c)
		}
	}
}

func (s *Server) handleAudio(ctx context.Context, sess *session, send func(outbound), payload []byte) {
	sess.mu.Lock()

	if !sess.listening {
		sess.mu.Unlock()
		return
	}
	now := time.Now()
	if rmsOf(payload) >= s.silenceRMS {
		sess.speechStarted = true
		sess.lastSpeechAt = now
	}
	sess.audio = append(sess.audio, payload...)
	for len(sess.audio) >= chunkBytes {
		chunk := append([]byte(nil), sess.audio[:chunkBytes]...)
		sess.audio = append([]byte(nil), sess.audio[chunkBytes:]...)
		seq := sess.nextSeq
		sess.nextSeq++
		sess.pending.Add(1)
		go s.transcribeChunk(sess.runCtx, sess, send, seq, chunk)
	}
	due := sess.speechStarted && now.Sub(sess.lastSpeechAt) >= s.silenceFor
	sess.mu.Unlock()
	if due {
		s.endUtterance(sess, send)
	}
}

func (s *Server) transcribeChunk(ctx context.Context, sess *session, send func(outbound), seq int, chunk []byte) {
	defer sess.pending.Done()
	text, err := s.transcriber.Transcribe(ctx, chunk)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		send(outbound{Type: "error", Text: "Whisper failed on one chunk, continuing: " + err.Error()})
		return
	}
	text = strings.TrimSpace(blankRe.ReplaceAllString(text, ""))
	if text == "" {
		return
	}
	sess.mu.Lock()
	sess.partials[seq] = text
	sess.mu.Unlock()
	send(outbound{Type: "partial", Seq: seq, Text: text})
}

func (s *Server) handleControl(ctx context.Context, sess *session, send func(outbound), c control) {
	switch c.Type {
	case "start":
		s.armListening(ctx, sess, send)
	case "end":
		s.endUtterance(sess, send)
	case "stop":
		sess.mu.Lock()
		sess.listening = false
		sess.armed = false
		sess.audio = sess.audio[:0]
		sess.partials = make(map[int]string)
		if sess.cancel != nil {
			sess.cancel()
		}
		sess.mu.Unlock()
		if s.cancelAgent(sess) {
			send(outbound{Type: "status", Text: "Stopped. Some files may have changed."})
		} else {
			send(outbound{Type: "status", Text: "Stopped. Nothing was kept."})
		}

	case "text":
		if clean.HasWakeWord(c.Text) {
			c.Text = clean.StripWakeWord(c.Text)
		}
		cleaned := clean.Clean(c.Text)
		send(outbound{Type: "cleaned", Text: cleaned})
		s.submit(ctx, sess, send, cleaned)
	default:
		send(outbound{Type: "error", Text: "unknown message type " + c.Type})
	}
}

// armListening begins (or restarts) accepting audio for the next utterance.
func (s *Server) armListening(ctx context.Context, sess *session, send func(outbound)) {
	sess.mu.Lock()
	if sess.cancel != nil {
		sess.cancel()
	}
	sess.runCtx, sess.cancel = context.WithCancel(ctx)
	sess.listening = true
	sess.armed = true
	sess.audio = sess.audio[:0]
	sess.partials = make(map[int]string)
	sess.nextSeq = 0
	sess.speechStarted = false
	sess.lastSpeechAt = time.Time{}
	runCtx := sess.runCtx
	sess.mu.Unlock()
	send(outbound{Type: "status", Text: "Listening for hey VoiceCoder…"})
	go s.watchSilence(runCtx, sess, send)
}

func (s *Server) finishUtterance(ctx context.Context, sess *session, send func(outbound)) {
	sess.pending.Wait()

	sess.mu.Lock()
	seqs := make([]int, 0, len(sess.partials))
	for seq := range sess.partials {
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)
	joined := ""
	for _, seq := range seqs {
		if joined != "" {
			joined += " "
		}
		joined += sess.partials[seq]
	}
	armed := sess.armed
	base := sess.baseCtx
	sess.mu.Unlock()

	rearm := func() {
		if !armed {
			return
		}
		if base == nil {
			base = context.Background()
		}
		s.armListening(base, sess, send)
	}

	if joined == "" {
		send(outbound{Type: "error", Text: "I did not hear anything. Try again, a little closer to the mic."})
		rearm()
		return
	}
	send(outbound{Type: "transcript", Text: joined})

	// Spoken requests need the wake word. Stop/cancel still work without it.
	if isStopWord(clean.StripWakeWord(joined)) || isStopWord(joined) {
		cleaned := clean.Clean(joined)
		send(outbound{Type: "cleaned", Text: cleaned})
		s.submit(ctx, sess, send, cleaned)
		rearm()
		return
	}
	if !clean.HasWakeWord(joined) {
		send(outbound{Type: "status", Text: "Say hey VoiceCoder, hey Coder, or Coder, then your request."})
		rearm()
		return
	}

	cleaned := clean.Clean(joined)
	send(outbound{Type: "cleaned", Text: cleaned})
	s.submit(ctx, sess, send, cleaned)
	rearm()
}

// cancelAgent stops the session's agent run and reports whether one was
// in progress.
func (s *Server) cancelAgent(sess *session) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.agentCancel == nil {
		return false
	}
	sess.agentCancel()
	sess.agentCancel = nil
	return true
}

// isStopWord reports whether text is just "stop" or "cancel", ignoring
// case, spaces, and trailing punctuation.
func isStopWord(text string) bool {
	word := strings.ToLower(strings.TrimRight(strings.TrimSpace(text), " .,;:!?"))
	return word == "stop" || word == "cancel"
}

// submit sends a cleaned request to the agent, one run per session at a
// time. It does nothing when no agent is set.
func (s *Server) submit(ctx context.Context, sess *session, send func(outbound), cleaned string) {
	if s.agent == nil || strings.TrimSpace(cleaned) == "" {
		return
	}

	sess.mu.Lock()
	if sess.agentCancel != nil {
		sess.mu.Unlock()
		if isStopWord(cleaned) {
			s.handleControl(ctx, sess, send, control{Type: "stop"})
			return
		}
		send(outbound{Type: "status", Text: "Still working on your last request. Say stop to cancel it."})
		return
	}
	base := sess.baseCtx
	if base == nil {
		base = context.Background()
	}
	agentCtx, cancel := context.WithCancel(base)
	sess.agentCancel = cancel
	sess.agentRun++
	run := sess.agentRun
	sess.mu.Unlock()

	send(outbound{Type: "status", Text: "Working on it."})
	go func() {
		type outcome struct {
			res agent.Result
			err error
		}
		done := make(chan outcome, 1)
		gotPartial := false
		var partialMu sync.Mutex
		go func() {
			res, err := s.agent.Run(agentCtx, cleaned, func(text string) {
				partialMu.Lock()
				gotPartial = true
				partialMu.Unlock()
				send(outbound{Type: "agent_partial", Text: text})
			})
			done <- outcome{res, err}
		}()

		// Say so every progressEvery while the run goes on with no partials,
		// so the user is not left in silence.
		started := time.Now()
		tick := time.NewTicker(s.progressEvery)
		defer tick.Stop()
		var out outcome
	wait:
		for {
			select {
			case out = <-done:
				break wait
			case <-agentCtx.Done():
				out = <-done
				break wait
			case <-tick.C:
				partialMu.Lock()
				has := gotPartial
				partialMu.Unlock()
				if has {
					continue
				}
				secs := int(time.Since(started).Round(time.Second).Seconds())
				send(outbound{Type: "status", Text: fmt.Sprintf("Still working, %d seconds so far.", secs)})
			}
		}
		res, err := out.res, out.err

		sess.mu.Lock()
		if sess.agentRun == run {
			sess.agentCancel = nil
		}
		sess.mu.Unlock()
		cancel()

		if errors.Is(err, context.Canceled) {
			send(outbound{Type: "error", Text: "Stopped."})
			return
		}
		if err != nil {
			send(outbound{Type: "error", Text: err.Error()})
			return
		}
		if s.repo != "" {
			if werr := agent.WriteLatestDiff(s.repo, res.Diff); werr != nil {
				log.Printf("write latest.diff: %v", werr)
			}
		}
		send(outbound{Type: "agent_result", Text: res.Summary, Diff: res.Diff})
	}()
}

func (s *Server) autoEndDue(sess *session, now time.Time) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.listening && sess.speechStarted && now.Sub(sess.lastSpeechAt) >= s.silenceFor
}

func (s *Server) watchSilence(ctx context.Context, sess *session, send func(outbound)) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			sess.mu.Lock()
			listening := sess.listening
			sess.mu.Unlock()
			if !listening {
				return
			}
			if s.autoEndDue(sess, now) {
				s.endUtterance(sess, send)
				return
			}
		}
	}
}

func (s *Server) endUtterance(sess *session, send func(outbound)) {
	sess.mu.Lock()
	if !sess.listening {
		sess.mu.Unlock()
		return
	}
	sess.listening = false
	if len(sess.audio) > 0 {
		chunk := append([]byte(nil), sess.audio...)
		sess.audio = sess.audio[:0]
		seq := sess.nextSeq
		sess.nextSeq++
		sess.pending.Add(1)
		go s.transcribeChunk(sess.runCtx, sess, send, seq, chunk)
	}
	runCtx := sess.runCtx
	sess.mu.Unlock()
	go s.finishUtterance(runCtx, sess, send)
}

func rmsOf(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}

	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}
