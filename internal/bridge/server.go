package bridge

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/theatlantadream/voicecoder-bridge/internal/agent"
	"github.com/theatlantadream/voicecoder-bridge/internal/chat"
	"github.com/theatlantadream/voicecoder-bridge/internal/clean"
	"github.com/theatlantadream/voicecoder-bridge/internal/tts"
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
	// endWord, when non-empty, ends an utterance as soon as a partial
	// transcript contains that whole word (in addition to silence).
	endWord string
	agent   agent.Agent
	repo    string
	origins []string
	speaker Speaker
	// intro is spoken once, right after the first request is sent to the
	// agent. introFile remembers that across restarts; empty keeps it in
	// memory only.
	intro     string
	introFile string
	introMu   sync.Mutex
	introDone bool
	// progressEvery is how often a running agent reports that it is still
	// working when no partials have arrived yet. Tests shorten it.
	progressEvery time.Duration
	// chatter, when set, narrates a running agent and answers the user
	// while it works. profileFile is the user's local profile, given to it
	// as background.
	chatter      Chatter
	profileFile  string
	profileMu    sync.Mutex
	narrateFirst time.Duration
	narrateEvery time.Duration
}

type Option func(*Server)

// DefaultIntro is the welcome message spoken after the first request.
const DefaultIntro = `Hey! Welcome to VoiceCoder! I'm Terrance, your personal VoiceCoder assistant, and I'm here to help you on your coding journey! To send a message to me, say: "Hey Coder!" or "Hey VoiceCoder!". Once you are done saying what you want to say, just say the word "Over!" and your message will be sent. If you ever wish to change my coding or messaging style, feel free to tell me your preferences!`

// WithIntro speaks text once, right after the first request goes to the
// agent. markerFile records that it was played so a restart does not repeat
// it; pass "" to remember only while the bridge runs. Needs WithSpeaker.
func WithIntro(text, markerFile string) Option {
	return func(s *Server) {
		s.intro = strings.TrimSpace(text)
		s.introFile = markerFile
		if markerFile != "" {
			if _, err := os.Stat(markerFile); err == nil {
				s.introDone = true
			}
		}
	}
}

// Speaker turns text into MP3 audio.
type Speaker interface {
	Speak(ctx context.Context, text string) ([]byte, error)
}

// WithSpeaker reads each agent summary aloud: after agent_result the bridge
// sends a "speech" message carrying the audio. Without it nothing is spoken.
func WithSpeaker(sp Speaker) Option {
	return func(s *Server) {
		s.speaker = sp
	}
}

func WithSilence(rms float64, d time.Duration) Option {
	return func(s *Server) {
		s.silenceRMS = rms
		s.silenceFor = d
	}
}

// WithEndWord ends a spoken utterance when this whole word appears in the
// live transcript (for example "over" or "send"). Empty disables it; silence
// still ends utterances as usual.
func WithEndWord(word string) Option {
	return func(s *Server) {
		s.endWord = strings.TrimSpace(word)
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
		narrateFirst:  3 * time.Second,
		narrateEvery:  9 * time.Second,
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
	// Audio is base64 MP3. Only set on a "speech" message.
	Audio string `json:"audio,omitempty"`
	// Kind is "intro", "summary", "narration" (said while the agent works)
	// or "reply" (an answer to the user mid-run). Only set on "speech".
	Kind string `json:"kind,omitempty"`
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
	// quietSpeech is true from the moment an agent run starts until its
	// agent_result/error has been sent. While set, speech partials (and
	// non-stop transcripts) are not streamed to the client.
	quietSpeech bool
	// speaking is true while the client plays a summary aloud (it says so
	// with speech_start / speech_end). Speech heard then is treated like
	// speech during an agent run: only an interrupt phrase or "stop" counts.
	speaking bool
	// bargeIn is true once "hey Coder" was heard during an agent run or a
	// spoken summary. The rest of that utterance becomes the next request.
	bargeIn bool
	// runDone is closed when the current agent run has fully ended.
	runDone chan struct{}
	// runPrompt and runPartial describe the run in progress, for narration
	// and for answering the user while it works.
	runPrompt  string
	runPartial string
	// sideTurns is the spoken side conversation held during agent runs.
	sideTurns []chat.Message
	// queued is a request the user gave mid-run, sent when the run ends.
	queued string
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
	endNow := s.endWord != "" && clean.HasEndWord(joinPartialsLocked(sess), s.endWord)
	// Keep listening for "stop" while an agent run is in progress, but do
	// not stream speech partials to the client until that run's result is sent.
	quiet := (sess.quietSpeech || sess.speaking) && !sess.bargeIn
	cutIn := quiet && clean.HasInterruptPhrase(joinPartialsLocked(sess))
	if cutIn {
		sess.bargeIn = true
		sess.speaking = false
		quiet = false
	}
	running := sess.agentCancel != nil
	sess.mu.Unlock()
	if cutIn {
		// Tell the client to stop talking right away. What was said is
		// handled when the utterance ends.
		send(outbound{Type: "interrupt"})
		switch {
		case running && s.chatter != nil:
			// The work carries on; the user gets an answer alongside it.
			send(outbound{Type: "status", Text: "Go ahead, I'm listening. I'm still working."})
		case s.cancelAgent(sess):
			send(outbound{Type: "status", Text: "Stopped. Some files may have changed. Go ahead, I'm listening."})
		}
	}
	if !quiet {
		send(outbound{Type: "partial", Seq: seq, Text: text})
	}
	if endNow {
		s.endUtterance(sess, send)
	}
}

// joinPartialsLocked joins partial transcripts in seq order. Caller must hold sess.mu.
func joinPartialsLocked(sess *session) string {
	seqs := make([]int, 0, len(sess.partials))
	for seq := range sess.partials {
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)
	var b strings.Builder
	for i, seq := range seqs {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(sess.partials[seq])
	}
	return b.String()
}

func (s *Server) handleControl(ctx context.Context, sess *session, send func(outbound), c control) {
	switch c.Type {
	case "start":
		s.armListening(ctx, sess, send)
	case "end":
		s.endUtterance(sess, send)
	case "speech_start":
		sess.mu.Lock()
		sess.speaking = true
		sess.mu.Unlock()
	case "speech_end":
		sess.mu.Lock()
		sess.speaking = false
		sess.mu.Unlock()
	case "stop":
		sess.mu.Lock()
		sess.queued = ""
		sess.speaking = false
		sess.bargeIn = false
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
		cleaned := clean.CleanEnd(c.Text, s.endWord)
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
	sess.bargeIn = false
	runCtx := sess.runCtx
	sess.mu.Unlock()
	send(outbound{Type: "status", Text: "Listening for hey VoiceCoder…"})
	go s.watchSilence(runCtx, sess, send)
}

func (s *Server) finishUtterance(ctx context.Context, sess *session, send func(outbound)) {
	sess.pending.Wait()

	sess.mu.Lock()
	joined := joinPartialsLocked(sess)
	armed := sess.armed
	base := sess.baseCtx
	bargeIn := sess.bargeIn
	speaking := sess.speaking
	quiet := (sess.quietSpeech || speaking) && !bargeIn
	running := sess.agentCancel != nil
	sideTalk := bargeIn && s.chatter != nil && running
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
		if quiet {
			// Ignore empty hush while the agent works; stay armed for "stop".
			rearm()
			return
		}
		send(outbound{Type: "error", Text: "I did not hear anything. Try again, a little closer to the mic."})
		rearm()
		return
	}

	// Spoken stop/cancel must work even while quietSpeech is set.
	if isStopWord(clean.StripWakeWord(joined)) || isStopWord(joined) {
		if speaking && !bargeIn && !running {
			// "Stop" during a spoken summary just ends the talking.
			sess.mu.Lock()
			sess.speaking = false
			sess.mu.Unlock()
			send(outbound{Type: "interrupt"})
			rearm()
			return
		}
		send(outbound{Type: "transcript", Text: joined})
		cleaned := clean.CleanEnd(joined, s.endWord)
		send(outbound{Type: "cleaned", Text: cleaned})
		s.submit(ctx, sess, send, cleaned)
		rearm()
		return
	}

	// While the agent is working, swallow other speech so the UI stays quiet.
	if quiet {
		rearm()
		return
	}

	send(outbound{Type: "transcript", Text: joined})

	if sideTalk {
		// Said to Terrance while the agent works: answer, keep working.
		cleaned := clean.CleanEnd(joined, s.endWord)
		send(outbound{Type: "cleaned", Text: cleaned})
		if base == nil {
			base = context.Background()
		}
		go s.sideTalk(base, sess, send, cleaned)
		rearm()
		return
	}

	if !clean.HasWakeWord(joined) {
		send(outbound{Type: "status", Text: "Say hey VoiceCoder, hey Coder, or Coder, then your request."})
		rearm()
		return
	}

	cleaned := clean.CleanEnd(joined, s.endWord)
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
	// quietSpeech stays set until endAgentQuiet so in-flight chunks cannot
	// emit partials during teardown.
	return true
}

const stillWorking = "Still working on your last request. Say stop to cancel it."

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
		if s.chatter != nil {
			go s.sideTalk(context.WithoutCancel(ctx), sess, send, cleaned)
			return
		}
		send(outbound{Type: "status", Text: stillWorking})
		return
	}
	base := sess.baseCtx
	if base == nil {
		base = context.Background()
	}
	agentCtx, cancel := context.WithCancel(base)
	prevDone := sess.runDone
	runDone := make(chan struct{})
	sess.runDone = runDone
	sess.agentCancel = cancel
	sess.quietSpeech = true
	sess.runPrompt, sess.runPartial = cleaned, ""
	sess.agentRun++
	run := sess.agentRun
	armed := sess.armed
	sess.mu.Unlock()

	send(outbound{Type: "status", Text: "Working on it."})
	go s.speakIntro(base, send)
	go func() {
		defer close(runDone)
		type outcome struct {
			res agent.Result
			err error
		}
		done := make(chan outcome, 1)
		nar := &narrator{}
		gotPartial := false
		var partialMu sync.Mutex
		go func() {
			// A run that was just cut in on must finish stopping first.
			if prevDone != nil {
				select {
				case <-prevDone:
				case <-agentCtx.Done():
					done <- outcome{err: context.Canceled}
					return
				}
			}
			res, err := s.agent.Run(agentCtx, cleaned, func(text string) {
				partialMu.Lock()
				gotPartial = true
				partialMu.Unlock()
				sess.mu.Lock()
				if sess.agentRun == run {
					sess.runPartial = text
				}
				sess.mu.Unlock()
				send(outbound{Type: "agent_partial", Text: text})
			})
			done <- outcome{res, err}
		}()

		// Say so every progressEvery while the run goes on with no partials,
		// so the user is not left in silence.
		started := time.Now()
		tick := time.NewTicker(s.progressEvery)
		defer tick.Stop()
		// With a chatter and a voice, say in plain words what is going on.
		var narrate <-chan time.Time
		if s.chatter != nil && s.speaker != nil {
			nt := time.NewTimer(s.narrateFirst)
			defer nt.Stop()
			narrate = nt.C
			defer nar.finished.Store(true)
			go func() {
				for {
					select {
					case <-agentCtx.Done():
						return
					case <-narrate:
						s.narrate(agentCtx, sess, send, nar, run)
						nt.Reset(s.narrateEvery)
					}
				}
			}()
		}
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
		nar.finished.Store(true)
		cancel()

		if errors.Is(err, context.Canceled) {
			send(outbound{Type: "error", Text: "Stopped."})
			s.endAgentQuiet(sess, run, armed, base, send)
			return
		}
		if err != nil {
			send(outbound{Type: "error", Text: err.Error()})
			s.endAgentQuiet(sess, run, armed, base, send)
			return
		}
		if s.repo != "" {
			if werr := agent.WriteLatestDiff(s.repo, res.Diff); werr != nil {
				log.Printf("write latest.diff: %v", werr)
			}
		}
		send(outbound{Type: "agent_result", Text: res.Summary, Diff: res.Diff})
		s.speak(base, send, "summary", tts.SpokenText(res.Summary))
		// Clear quietSpeech only after the result is on the wire so partials
		// cannot sneak in while the UI still shows the agent working.
		s.endAgentQuiet(sess, run, armed, base, send)
	}()
}

// endAgentQuiet marks the agent run finished and re-opens listening for the
// next request. quietSpeech stays set until this runs so late partials from
// the previous utterance never reach the client mid-result.
func (s *Server) endAgentQuiet(sess *session, run int, armed bool, base context.Context, send func(outbound)) {
	sess.mu.Lock()
	if sess.agentRun == run {
		sess.agentCancel = nil
		sess.quietSpeech = false
	}
	// A cut-in utterance is still being heard; re-arming would throw it away.
	stillArmed := armed && sess.armed && !sess.bargeIn
	queued := ""
	if sess.agentCancel == nil {
		queued, sess.queued = sess.queued, ""
	}
	sess.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	if stillArmed {
		s.armListening(base, sess, send)
	}
	if queued != "" {
		send(outbound{Type: "cleaned", Text: queued})
		s.submit(base, sess, send, queued)
	}
}

// speak sends text as a "speech" message. The result is already on screen, so
// a failure here is logged and otherwise ignored.
func (s *Server) speak(ctx context.Context, send func(outbound), kind, text string) bool {
	if s.speaker == nil || strings.TrimSpace(text) == "" {
		return false
	}
	audio, err := s.speaker.Speak(ctx, text)
	if err != nil {
		log.Printf("speech: %v", err)
		return false
	}
	if len(audio) == 0 {
		return false
	}
	send(outbound{Type: "speech", Kind: kind, Text: text, Audio: base64.StdEncoding.EncodeToString(audio)})
	return true
}

// speakIntro plays the welcome message the first time a request reaches the
// agent. It is claimed up front so two quick requests cannot both play it,
// and released again if the audio could not be made.
func (s *Server) speakIntro(ctx context.Context, send func(outbound)) {
	if s.speaker == nil || s.intro == "" {
		return
	}
	s.introMu.Lock()
	if s.introDone {
		s.introMu.Unlock()
		return
	}
	s.introDone = true
	s.introMu.Unlock()

	if !s.speak(ctx, send, "intro", s.intro) {
		s.introMu.Lock()
		s.introDone = false
		s.introMu.Unlock()
		return
	}
	if s.introFile == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.introFile), 0o755); err != nil {
		log.Printf("intro marker: %v", err)
		return
	}
	if err := os.WriteFile(s.introFile, []byte("played\n"), 0o644); err != nil {
		log.Printf("intro marker: %v", err)
	}
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
