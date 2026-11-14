package bridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/theatlantadream/voicecoder-bridge/internal/clean"
	"github.com/theatlantadream/voicecoder-bridge/internal/whisper"
)

const chunkBytes = 64000

var blankRe = regexp.MustCompile(`\[.*?\]`)

type Server struct {
	transcriber whisper.Transcriber
	upgrader    websocket.Upgrader
	silenceRMS  float64
	silenceFor  time.Duration
}

type Option func(*Server)

func WithSilence(rms float64, d time.Duration) Option {
	return func(s *Server) {
		s.silenceRMS = rms
		s.silenceFor = d
	}
}

func NewServer(t whisper.Transcriber, opts ...Option) *Server {
	s := &Server{
		transcriber: t,
		silenceRMS:  500,
		silenceFor:  1500 * time.Millisecond,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

type control struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type outbound struct {
	Type string `json:"type"`
	Seq  int    `json:"seq"`
	Text string `json:"text,omitempty"`
}

type session struct {
	mu            sync.Mutex
	listening     bool
	audio         []byte
	partials      map[int]string
	nextSeq       int
	pending       sync.WaitGroup
	runCtx        context.Context
	cancel        context.CancelFunc
	speechStarted bool
	lastSpeechAt  time.Time
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade: %v", err)
		return
	}

	defer conn.Close()

	sess := &session{partials: make(map[int]string)}
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
		sess.mu.Lock()
		if sess.cancel != nil {
			sess.cancel()
		}
		sess.runCtx, sess.cancel = context.WithCancel(ctx)
		sess.listening = true
		sess.audio = sess.audio[:0]
		sess.partials = make(map[int]string)
		sess.nextSeq = 0
		sess.speechStarted = false
		sess.lastSpeechAt = time.Time{}
		runCtx := sess.runCtx
		sess.mu.Unlock()
		go s.watchSilence(runCtx, sess, send)
	case "end":
		s.endUtterance(sess, send)
	case "stop":
		sess.mu.Lock()
		sess.listening = false
		sess.audio = sess.audio[:0]
		sess.partials = make(map[int]string)
		if sess.cancel != nil {
			sess.cancel()
		}
		sess.mu.Unlock()
		send(outbound{Type: "status", Text: "Stopped. Nothing was kept."})

	case "text":
		if clean.HasWakeWord(c.Text) {
			c.Text = clean.StripWakeWord(c.Text)
		}
		cleaned := clean.Clean(c.Text)
		send(outbound{Type: "cleaned", Text: cleaned})
	default:
		send(outbound{Type: "error", Text: "unknown message type " + c.Type})
	}
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
	sess.mu.Unlock()

	if joined == "" {
		send(outbound{Type: "error", Text: "I did not hear anything. Try again, a little closer to the mic."})
		return
	}
	send(outbound{Type: "transcript", Text: joined})

	cleaned := clean.Clean(joined)
	send(outbound{Type: "cleaned", Text: cleaned})
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
