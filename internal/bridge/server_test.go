package bridge

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/theatlantadream/voicecoder-bridge/internal/agent"
	"github.com/theatlantadream/voicecoder-bridge/internal/whisper"
)

// fakeTranscriber returns one canned line per chunk, so the test can check
// joining without a running Whisper server.
type fakeTranscriber struct {
	lines []string
	i     int
}

func (f *fakeTranscriber) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if f.i >= len(f.lines) {
		return "", nil
	}
	line := f.lines[f.i]
	f.i++
	return line, nil
}

var _ whisper.Transcriber = (*fakeTranscriber)(nil)

// TestFinishJoinsPartialsInOrder checks the core contract Stephen asked
// for: partials are joined in sequence order at "end", and cleaning
// happens once on the joined text, not per partial.
func TestFinishJoinsPartialsInOrder(t *testing.T) {
	srv := NewServer(&fakeTranscriber{})

	// Simulate three chunks that finished out of order: seq 1 arrived
	// before seq 0. The join must still read 0, 1, 2.
	sess := &session{partials: map[int]string{
		1: "the login bug",
		0: "hey VoiceCoder fix",
		2: "in auth dot py",
	}}

	var got []outbound
	send := func(m outbound) { got = append(got, m) }

	srv.finishUtterance(context.Background(), sess, send)

	var transcript, cleaned string
	for _, m := range got {
		switch m.Type {
		case "transcript":
			transcript = m.Text
		case "cleaned":
			cleaned = m.Text
		}
	}
	if transcript != "hey VoiceCoder fix the login bug in auth dot py" {
		t.Errorf("transcript = %q, want the three partials joined in order", transcript)
	}
	// Cleaning runs once, on the joined text: wake word stripped, and
	// "auth dot py" normalised, only here — not on any single partial.
	if cleaned != "fix the login bug in auth.py" {
		t.Errorf("cleaned = %q, want %q", cleaned, "fix the login bug in auth.py")
	}
}

// TestTranscribeChunkDropsBlankAudio checks that a chunk Whisper marks as
// non-speech ("[BLANK_AUDIO]") is never stored as a partial or sent to the
// frontend, so it cannot leak into the joined transcript.
func TestTranscribeChunkDropsBlankAudio(t *testing.T) {
	srv := NewServer(&fakeTranscriber{lines: []string{"[BLANK_AUDIO]"}})
	sess := &session{partials: make(map[int]string)}

	var got []outbound
	send := func(m outbound) { got = append(got, m) }

	sess.pending.Add(1) // transcribeChunk always defers Done().
	srv.transcribeChunk(context.Background(), sess, send, 0, []byte{1, 2, 3})
	sess.pending.Wait()

	if len(sess.partials) != 0 {
		t.Errorf("partials = %v, want none stored for a blank chunk", sess.partials)
	}
	if len(got) != 0 {
		t.Errorf("sent %d messages, want none for a blank chunk", len(got))
	}
}

// TestRMSOf checks the loudness measure behind hands-free ending:
// silence is 0, a steady tone reads back its own amplitude.
func TestRMSOf(t *testing.T) {
	if got := rmsOf([]byte{0, 0, 0, 0}); got != 0 {
		t.Errorf("rmsOf(silence) = %v, want 0", got)
	}
	pcm := make([]byte, 8) // four samples of +1000
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint16(pcm[2*i:], 1000)
	}
	if got := rmsOf(pcm); got != 1000 {
		t.Errorf("rmsOf(1000) = %v, want 1000", got)
	}
}

// TestAutoEndDue checks the hands-free ending rule: it fires only after
// speech has started and the silence has lasted long enough. Silence
// before any speech never ends an utterance.
func TestAutoEndDue(t *testing.T) {
	srv := NewServer(&fakeTranscriber{})
	now := time.Now()

	sess := &session{listening: true, speechStarted: false, lastSpeechAt: now.Add(-10 * time.Second)}
	if srv.autoEndDue(sess, now) {
		t.Errorf("autoEndDue = true before any speech, want false")
	}

	sess.speechStarted = true
	sess.lastSpeechAt = now.Add(-500 * time.Millisecond)
	if srv.autoEndDue(sess, now) {
		t.Errorf("autoEndDue = true after 0.5s of silence, want false (timeout is 1.5s)")
	}

	sess.lastSpeechAt = now.Add(-2 * time.Second)
	if !srv.autoEndDue(sess, now) {
		t.Errorf("autoEndDue = false after 2s of silence, want true")
	}

	sess.listening = false
	if srv.autoEndDue(sess, now) {
		t.Errorf("autoEndDue = true after the utterance ended, want false")
	}
}

// stubAgent stands in for a coding agent. Each run reports on started,
// then waits for release or for the run to be cancelled.
type stubAgent struct {
	started chan string
	stopped chan struct{}
	release chan struct{}
	result  agent.Result
	stream  bool // when true, emit an agent_partial via onUpdate
}

func newStubAgent() *stubAgent {
	return &stubAgent{
		started: make(chan string, 8),
		stopped: make(chan struct{}, 8),
		release: make(chan struct{}),
		result:  agent.Result{Summary: "Added the function.", Diff: "diff --git a/a.go b/a.go"},
	}
}

func (a *stubAgent) Run(ctx context.Context, prompt string, onUpdate agent.UpdateFunc) (agent.Result, error) {
	a.started <- prompt
	if a.stream && onUpdate != nil {
		onUpdate("Working…")
	}
	select {
	case <-ctx.Done():
		a.stopped <- struct{}{}
		return agent.Result{}, ctx.Err()
	case <-a.release:
		return a.result, nil
	}
}

var _ agent.Agent = (*stubAgent)(nil)

// collector is a goroutine-safe send, needed once an agent run replies
// from its own goroutine.
type collector struct {
	mu   sync.Mutex
	msgs []outbound
}

func (c *collector) send(m outbound) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
}

func (c *collector) all() []outbound {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]outbound(nil), c.msgs...)
}

func (c *collector) has(msgType, text string) bool {
	for _, m := range c.all() {
		if m.Type == msgType && m.Text == text {
			return true
		}
	}
	return false
}

// waitFor waits until a message of the given type arrives and returns it.
func (c *collector) waitFor(t *testing.T, msgType string) outbound {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range c.all() {
			if m.Type == msgType {
				return m
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %q message arrived, got %+v", msgType, c.all())
	return outbound{}
}

func waitStarted(t *testing.T, a *stubAgent) string {
	t.Helper()
	select {
	case prompt := <-a.started:
		return prompt
	case <-time.After(2 * time.Second):
		t.Fatal("the agent run never started")
		return ""
	}
}

func agentRunning(sess *session) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.agentCancel != nil
}

const (
	stopWithChanges = "Stopped. Some files may have changed."
	busyStatus      = "Still working on your last request. Say stop to cancel it."
)

// TestTypedRequestRunsAgent checks the typed path end to end: cleaned,
// then "Working on it.", streaming partial, then the agent's summary and diff.
func TestTypedRequestRunsAgent(t *testing.T) {
	stub := newStubAgent()
	stub.stream = true
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "hey VoiceCoder add a function in auth dot py"})
	if prompt := waitStarted(t, stub); prompt != "add a function in auth.py" {
		t.Errorf("agent prompt = %q, want the cleaned text", prompt)
	}
	close(stub.release)
	res := c.waitFor(t, "agent_result")

	if res.Text != stub.result.Summary || res.Diff != stub.result.Diff {
		t.Errorf("agent_result = %+v, want the agent's summary and diff", res)
	}
	if !c.has("status", "Working on it.") || !c.has("agent_partial", "Working…") {
		t.Errorf("messages = %+v, want Working on it. and an agent_partial", c.all())
	}
	if agentRunning(sess) {
		t.Errorf("session still marks a run in progress after the result")
	}
}

// TestSpokenWithoutWakeWord ignores speech that lacks the wake word.
func TestSpokenWithoutWakeWord(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub))
	sess := &session{partials: map[int]string{0: "fix the login bug"}, armed: true, baseCtx: context.Background()}
	c := &collector{}

	srv.finishUtterance(context.Background(), sess, c.send)
	if !c.has("status", "Say hey VoiceCoder, hey Coder, or Coder, then your request.") {
		t.Errorf("messages = %+v, want the wake-word reminder", c.all())
	}
	select {
	case prompt := <-stub.started:
		t.Errorf("agent got %q, want nothing without the wake word", prompt)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestSpokenRequestRunsAgent checks the voice path reaches the agent too.
func TestSpokenRequestRunsAgent(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub))
	sess := &session{partials: map[int]string{0: "hey VoiceCoder fix the login bug"}, armed: true, baseCtx: context.Background()}
	c := &collector{}

	srv.finishUtterance(context.Background(), sess, c.send)
	if prompt := waitStarted(t, stub); prompt != "fix the login bug" {
		t.Errorf("agent prompt = %q, want the cleaned text", prompt)
	}
	close(stub.release)
	c.waitFor(t, "agent_result")
}

// TestSpokenEndWordStripsTail drops the end word and anything after it.
func TestSpokenEndWordStripsTail(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithEndWord("over"))
	sess := &session{
		partials: map[int]string{0: "hey Coder fix the login bug over thanks"},
		armed:    true,
		baseCtx:  context.Background(),
	}
	c := &collector{}

	srv.finishUtterance(context.Background(), sess, c.send)
	if prompt := waitStarted(t, stub); prompt != "fix the login bug" {
		t.Errorf("agent prompt = %q, want end word stripped", prompt)
	}
	close(stub.release)
	c.waitFor(t, "agent_result")
}

// TestPartialEndWordEndsUtterance finishes as soon as a partial contains the end word.
func TestPartialEndWordEndsUtterance(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{lines: []string{"over"}}, WithAgent(stub), WithEndWord("over"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := &session{
		listening: true,
		armed:     true,
		partials:  map[int]string{0: "hey Coder add a test"},
		runCtx:    ctx,
		cancel:    cancel,
		baseCtx:   context.Background(),
	}
	c := &collector{}

	sess.pending.Add(1)
	srv.transcribeChunk(ctx, sess, c.send, 1, make([]byte, 4))
	if prompt := waitStarted(t, stub); prompt != "add a test" {
		t.Errorf("agent prompt = %q, want request before end word", prompt)
	}
	close(stub.release)
	c.waitFor(t, "agent_result")
}

// TestSecondRequestWhileBusy checks a request during a run is refused
// with the busy status and never reaches the agent.
func TestSecondRequestWhileBusy(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a test"})
	waitStarted(t, stub)
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "rename the file"})

	if !c.has("status", busyStatus) {
		t.Errorf("messages = %+v, want the busy status", c.all())
	}
	select {
	case prompt := <-stub.started:
		t.Errorf("second request %q reached the agent, want it refused", prompt)
	default:
	}
	close(stub.release)
	c.waitFor(t, "agent_result")
}

// TestStopCancelsRun checks the stop control cancels a run and warns that
// files may have changed.
func TestStopCancelsRun(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a test"})
	waitStarted(t, stub)
	srv.handleControl(context.Background(), sess, c.send, control{Type: "stop"})

	if !c.has("status", stopWithChanges) {
		t.Errorf("messages = %+v, want the files-may-have-changed stop status", c.all())
	}
	if c.has("status", "Stopped. Nothing was kept.") {
		t.Errorf("got the no-run stop wording while a run was in progress")
	}
	if e := c.waitFor(t, "error"); e.Text != "Stopped." {
		t.Errorf("error = %q, want the cancelled run to report Stopped.", e.Text)
	}
	if agentRunning(sess) {
		t.Errorf("session still marks a run in progress after stop")
	}
	for _, m := range c.all() {
		if m.Type == "agent_result" {
			t.Errorf("got an agent_result from a stopped run")
		}
	}
}

// TestStopWithoutRunKeepsOldWording checks stop with an agent set but no
// run in progress still says nothing was kept.
func TestStopWithoutRunKeepsOldWording(t *testing.T) {
	srv := NewServer(&fakeTranscriber{}, WithAgent(newStubAgent()))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "stop"})

	got := c.all()
	if len(got) != 1 || got[0] != (outbound{Type: "status", Text: "Stopped. Nothing was kept."}) {
		t.Errorf("messages = %+v, want only the old stop status", got)
	}
}

// TestNoPartialsWhileAgentBusy keeps live speech off the wire during a run
// so the UI is not flooded with partials; transcription still runs so "stop" works.
func TestNoPartialsWhileAgentBusy(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{lines: []string{"Coder do something else"}}, WithAgent(stub))
	sess := &session{partials: make(map[int]string), armed: true, baseCtx: context.Background()}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a test"})
	waitStarted(t, stub)
	if !sess.quietSpeech {
		t.Fatal("quietSpeech should be set once the agent run starts")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.mu.Lock()
	sess.listening = true
	sess.runCtx = ctx
	sess.cancel = cancel
	sess.mu.Unlock()

	sess.pending.Add(1)
	srv.transcribeChunk(ctx, sess, c.send, 0, make([]byte, 4))

	for _, m := range c.all() {
		if m.Type == "partial" {
			t.Fatalf("got partial %q while agent busy; want none", m.Text)
		}
	}
	sess.mu.Lock()
	stored := sess.partials[0]
	sess.mu.Unlock()
	if stored != "Coder do something else" {
		t.Errorf("partials[0] = %q, want transcript kept for stop/finish", stored)
	}

	// Non-stop speech while busy must not produce transcript/cleaned either.
	sess.mu.Lock()
	sess.partials = map[int]string{0: "Coder do another thing"}
	sess.mu.Unlock()
	before := len(c.all())
	srv.finishUtterance(context.Background(), sess, c.send)
	for _, m := range c.all()[before:] {
		if m.Type == "partial" || m.Type == "transcript" || m.Type == "cleaned" {
			t.Fatalf("got %s %q while agent busy; want silence", m.Type, m.Text)
		}
	}

	close(stub.release)
	c.waitFor(t, "agent_result")
	if sess.quietSpeech {
		t.Fatal("quietSpeech should clear after agent_result")
	}
}

// TestSpokenStopWhileBusy checks that saying "Stop." during a run cancels
// it instead of being sent to the agent as a task.
func TestSpokenStopWhileBusy(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a test"})
	waitStarted(t, stub)

	sess.mu.Lock()
	sess.partials = map[int]string{0: "Stop."}
	sess.mu.Unlock()
	srv.finishUtterance(context.Background(), sess, c.send)

	if !c.has("status", stopWithChanges) {
		t.Errorf("messages = %+v, want the run stopped", c.all())
	}
	c.waitFor(t, "error")
	if agentRunning(sess) {
		t.Errorf("session still marks a run in progress after a spoken stop")
	}
}

func TestIsStopWord(t *testing.T) {
	for _, text := range []string{"stop", "Stop.", "STOP", "stop!", "  cancel  ", "Cancel."} {
		if !isStopWord(text) {
			t.Errorf("isStopWord(%q) = false, want true", text)
		}
	}
	for _, text := range []string{"", "stop the server", "don't stop", "cancel the last change"} {
		if isStopWord(text) {
			t.Errorf("isStopWord(%q) = true, want false", text)
		}
	}
}

// TestStartDoesNotCancelRun checks a new utterance leaves a running agent
// alone: start cancels only transcription.
func TestStartDoesNotCancelRun(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // ends the silence watcher that start launches

	srv.handleControl(ctx, sess, c.send, control{Type: "text", Text: "add a test"})
	waitStarted(t, stub)
	srv.handleControl(ctx, sess, c.send, control{Type: "start"})

	if !agentRunning(sess) {
		t.Fatalf("start cancelled the agent run")
	}
	close(stub.release)
	if res := c.waitFor(t, "agent_result"); res.Text != stub.result.Summary {
		t.Errorf("agent_result = %+v, want the run to finish normally", res)
	}
	for _, m := range c.all() {
		if m.Type == "error" {
			t.Errorf("got error %q, want none", m.Text)
		}
	}
}

// TestNoAgentUnchanged checks that without an agent a typed request gives
// only "cleaned", as before, and starts nothing.
func TestNoAgentUnchanged(t *testing.T) {
	srv := NewServer(&fakeTranscriber{})
	sess := &session{partials: make(map[int]string)}

	var got []outbound
	send := func(m outbound) { got = append(got, m) }

	srv.handleControl(context.Background(), sess, send, control{Type: "text", Text: "add a test"})

	if len(got) != 1 || got[0] != (outbound{Type: "cleaned", Text: "add a test"}) {
		t.Errorf("messages = %+v, want only cleaned", got)
	}
	if agentRunning(sess) {
		t.Errorf("a run was started with no agent set")
	}
}

func TestOriginCheck(t *testing.T) {
	withAgent := NewServer(&fakeTranscriber{}, WithAgent(newStubAgent()),
		WithAllowedOrigins([]string{"http://localhost", "http://127.0.0.1", "https://coder.example.com", "https://*.apps.example.org"}))
	noAgent := NewServer(&fakeTranscriber{})

	cases := []struct {
		name   string
		srv    *Server
		origin string
		want   bool
	}{
		{"allowed origin", withAgent, "https://coder.example.com", true},
		{"localhost on any port", withAgent, "http://localhost:8090", true},
		{"127.0.0.1 on any port", withAgent, "http://127.0.0.1:5173", true},
		{"missing origin", withAgent, "", true},
		{"other origin", withAgent, "https://evil.example.com", false},
		{"allowed host on another port", withAgent, "https://coder.example.com:8443", false},
		{"allowed host with the wrong scheme", withAgent, "http://coder.example.com", false},
		{"host that only starts like localhost", withAgent, "http://localhost.evil.example.com", false},
		{"page opened as a file", withAgent, "null", false},
		{"subdomain of a wildcard origin", withAgent, "https://frontend--ws--user.apps.example.org", true},
		{"wildcard does not match the bare domain", withAgent, "https://apps.example.org", false},
		{"wildcard does not match a lookalike suffix", withAgent, "https://evilapps.example.org", false},
		{"wildcard with the wrong scheme", withAgent, "http://x.apps.example.org", false},
		{"same host the page connects to", withAgent, "https://bridge.test", true},
		{"same host with different case", withAgent, "https://BRIDGE.test", true},
		{"same host name on another port", withAgent, "https://bridge.test:8443", false},
		{"no agent allows every origin", noAgent, "https://evil.example.com", true},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://bridge.test/ws", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := tc.srv.originAllowed(r); got != tc.want {
			t.Errorf("%s: originAllowed(%q) = %v, want %v", tc.name, tc.origin, got, tc.want)
		}
	}
}

// TestWebSocketOriginAndClose goes through a real connection: a page from
// another origin is refused, and closing the socket cancels a running task.
func TestWebSocketOriginAndClose(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithAllowedOrigins([]string{"http://localhost"}))
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleWS))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	_, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {"https://evil.example.com"}})
	if err == nil {
		t.Fatalf("a connection from another origin was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("other origin: response = %v, want 403", resp)
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {"http://localhost:8090"}})
	if err != nil {
		t.Fatalf("a connection from localhost was refused: %v", err)
	}
	if err := conn.WriteJSON(control{Type: "text", Text: "add a test"}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, stub)
	conn.Close()

	select {
	case <-stub.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the socket did not cancel the running task")
	}
}

// TestProgressWhileAgentWorks checks a slow run reports progress before
// its result and never after it.
func TestProgressWhileAgentWorks(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub))
	srv.progressEvery = 10 * time.Millisecond
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	isProgress := func(m outbound) bool {
		return m.Type == "status" && strings.HasPrefix(m.Text, "Still working, ")
	}
	countProgress := func() int {
		n := 0
		for _, m := range c.all() {
			if isProgress(m) {
				n++
			}
		}
		return n
	}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a test"})
	waitStarted(t, stub)
	deadline := time.Now().Add(2 * time.Second)
	for countProgress() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no progress status while the agent was working, got %+v", c.all())
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stub.release)
	c.waitFor(t, "agent_result")

	time.Sleep(10 * srv.progressEvery)
	got := c.all()
	if last := got[len(got)-1]; last.Type != "agent_result" {
		t.Errorf("last message = %+v, want nothing after agent_result", last)
	}
	if first := got[2]; !isProgress(first) || !strings.HasSuffix(first.Text, " seconds so far.") {
		t.Errorf("first progress = %+v, want \"Still working, N seconds so far.\"", first)
	}
}

// TestProgressStopsOnStop checks no progress is sent once a run is stopped.
func TestProgressStopsOnStop(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub))
	srv.progressEvery = 10 * time.Millisecond
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a test"})
	waitStarted(t, stub)
	srv.handleControl(context.Background(), sess, c.send, control{Type: "stop"})
	c.waitFor(t, "error")

	n := len(c.all())
	time.Sleep(10 * srv.progressEvery)
	if got := c.all(); len(got) != n {
		t.Errorf("messages after the run was stopped: %+v", got[n:])
	}
}

type stubSpeaker struct {
	mu    sync.Mutex
	texts []string
	audio []byte
	err   error
}

func (s *stubSpeaker) Speak(ctx context.Context, text string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.texts = append(s.texts, text)
	return s.audio, s.err
}

// TestResultIsSpoken sends the summary as audio right after agent_result.
func TestResultIsSpoken(t *testing.T) {
	stub := newStubAgent()
	speaker := &stubSpeaker{audio: []byte("mp3")}
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithSpeaker(speaker))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	waitStarted(t, stub)
	close(stub.release)
	speech := c.waitFor(t, "speech")

	if speech.Text != stub.result.Summary || speech.Audio != base64.StdEncoding.EncodeToString([]byte("mp3")) {
		t.Errorf("speech = %+v, want the summary text and base64 audio", speech)
	}
	got := c.all()
	if n := len(got); n < 2 || got[n-2].Type != "agent_result" || got[n-1].Type != "speech" {
		t.Errorf("messages = %+v, want agent_result then speech last", got)
	}
	if len(speaker.texts) != 1 || speaker.texts[0] != stub.result.Summary {
		t.Errorf("spoken = %q, want only the summary", speaker.texts)
	}
}

// TestSpeechFailureKeepsResult still delivers the result when speech fails.
func TestSpeechFailureKeepsResult(t *testing.T) {
	stub := newStubAgent()
	speaker := &stubSpeaker{err: errors.New("quota exceeded")}
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithSpeaker(speaker))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	waitStarted(t, stub)
	close(stub.release)
	c.waitFor(t, "agent_result")
	time.Sleep(50 * time.Millisecond)

	for _, m := range c.all() {
		if m.Type == "speech" || m.Type == "error" {
			t.Errorf("got %+v, want no speech and no error shown to the user", m)
		}
	}
}

// TestStoppedRunIsNotSpoken says nothing aloud for a run that was stopped.
func TestStoppedRunIsNotSpoken(t *testing.T) {
	stub := newStubAgent()
	speaker := &stubSpeaker{audio: []byte("mp3")}
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithSpeaker(speaker))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	waitStarted(t, stub)
	srv.handleControl(context.Background(), sess, c.send, control{Type: "stop"})
	c.waitFor(t, "error")
	time.Sleep(50 * time.Millisecond)

	speaker.mu.Lock()
	defer speaker.mu.Unlock()
	if len(speaker.texts) != 0 {
		t.Errorf("spoken = %q, want nothing for a stopped run", speaker.texts)
	}
}

func introCount(sp *stubSpeaker) int {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	n := 0
	for _, text := range sp.texts {
		if text == "welcome" {
			n++
		}
	}
	return n
}

// TestIntroSpokenOnce plays the welcome after the first request only, and
// remembers it in the marker file across bridge restarts.
func TestIntroSpokenOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "state", "intro-played")
	sp := &stubSpeaker{audio: []byte("mp3")}
	run := func() {
		srv := NewServer(&fakeTranscriber{}, WithAgent(agent.Fake{}), WithSpeaker(sp), WithIntro("welcome", marker))
		sess := &session{partials: make(map[int]string)}
		for i := 0; i < 2; i++ {
			c := &collector{}
			srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
			c.waitFor(t, "agent_result")
			c.waitFor(t, "speech")
		}
	}
	run()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker file not written: %v", err)
	}
	if got := introCount(sp); got != 1 {
		t.Fatalf("intro spoken %d times in the first bridge, want 1", got)
	}
	run()
	if got := introCount(sp); got != 1 {
		t.Fatalf("intro spoken %d times after a restart, want 1", got)
	}
}

// TestIntroRetriedAfterFailure does not mark the intro played when the
// audio could not be made.
func TestIntroRetriedAfterFailure(t *testing.T) {
	sp := &stubSpeaker{err: errors.New("elevenlabs down")}
	srv := NewServer(&fakeTranscriber{}, WithAgent(agent.Fake{}), WithSpeaker(sp), WithIntro("welcome", ""))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	c.waitFor(t, "agent_result")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.introMu.Lock()
		done := srv.introDone
		srv.introMu.Unlock()
		if introCount(sp) > 0 && !done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("intro stayed marked as played after a failed attempt")
}

type replyAgent struct{ reply string }

func (a replyAgent) Run(ctx context.Context, prompt string, onUpdate agent.UpdateFunc) (agent.Result, error) {
	return agent.Result{Summary: a.reply}, nil
}

// TestOnlySummaryIsSpoken reads aloud the Summary section, not the code,
// while the on-screen result keeps the whole reply.
func TestOnlySummaryIsSpoken(t *testing.T) {
	sp := &stubSpeaker{audio: []byte("mp3")}
	srv := NewServer(&fakeTranscriber{}, WithAgent(replyAgent{"Here:\n```go\nfunc a() {}\n```\nSummary: I added a function."}), WithSpeaker(sp))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	if res := c.waitFor(t, "agent_result"); !strings.Contains(res.Text, "func a()") {
		t.Fatalf("on-screen result lost the code: %q", res.Text)
	}
	if got := c.waitFor(t, "speech"); got.Text != "I added a function." {
		t.Fatalf("spoken %q, want only the summary", got.Text)
	}
}

// hear feeds one transcribed chunk into the session, as live audio would.
func hear(srv *Server, sess *session, c *collector, seq int) {
	sess.pending.Add(1)
	srv.transcribeChunk(context.Background(), sess, c.send, seq, []byte{0, 0})
}

// TestHeyCoderCutsInOnRun stops the running agent as soon as "hey Coder" is
// heard, then sends the rest of that utterance as the next request.
func TestHeyCoderCutsInOnRun(t *testing.T) {
	stub := newStubAgent()
	tr := &fakeTranscriber{lines: []string{"some chatter", "Hey, Coder", "add a test instead over"}}
	srv := NewServer(tr, WithAgent(stub), WithEndWord("over"))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	waitStarted(t, stub)

	hear(srv, sess, c, 0)
	if c.has("partial", "some chatter") || len(stub.stopped) != 0 {
		t.Fatalf("chatter without hey Coder reached the client or stopped the run: %+v", c.all())
	}
	hear(srv, sess, c, 1)
	c.waitFor(t, "interrupt")
	select {
	case <-stub.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("run was not stopped by hey Coder")
	}
	c.waitFor(t, "error") // the old run reports it stopped
	sess.mu.Lock()
	kept := len(sess.partials)
	sess.mu.Unlock()
	if kept != 2 {
		t.Fatalf("partials kept = %d, want the cut-in utterance left intact", kept)
	}

	sess.pending.Add(1)
	srv.transcribeChunk(context.Background(), sess, c.send, 2, []byte{0, 0})
	srv.finishUtterance(context.Background(), sess, c.send)
	select {
	case prompt := <-stub.started:
		if prompt != "add a test instead" {
			t.Errorf("new request = %q, want %q", prompt, "add a test instead")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no new run after the cut-in: %+v", c.all())
	}
	close(stub.release)
	c.waitFor(t, "agent_result")
}

// TestBareCoderDoesNotCutIn needs "hey" to interrupt work in progress.
func TestBareCoderDoesNotCutIn(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{lines: []string{"the coder is busy"}}, WithAgent(stub))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	waitStarted(t, stub)
	hear(srv, sess, c, 0)
	srv.finishUtterance(context.Background(), sess, c.send)
	if c.has("interrupt", "") || len(stub.stopped) != 0 || !agentRunning(sess) {
		t.Errorf("run was interrupted without hey: %+v", c.all())
	}
	close(stub.release)
}

// TestCutInOnSpokenSummary covers speech heard while the client is talking:
// chatter is ignored, "stop" ends the talking, "hey Coder" starts a request.
func TestCutInOnSpokenSummary(t *testing.T) {
	stub := newStubAgent()
	tr := &fakeTranscriber{lines: []string{"I added the function", "Stop.", "hey VoiceCoder rename it"}}
	srv := NewServer(tr, WithAgent(stub))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}
	next := func() {
		sess.mu.Lock()
		sess.partials = make(map[int]string)
		sess.bargeIn = false
		sess.mu.Unlock()
		hear(srv, sess, c, 0)
		srv.finishUtterance(context.Background(), sess, c.send)
	}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "speech_start"})
	next() // the app's own voice leaking into the mic
	if got := c.all(); len(got) != 0 {
		t.Fatalf("echo of the summary produced messages: %+v", got)
	}
	next() // "Stop."
	if !c.has("interrupt", "") || len(stub.started) != 0 {
		t.Fatalf("stop during speech: %+v, want interrupt and no agent run", c.all())
	}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "speech_start"})
	before := len(c.all())
	next() // "hey VoiceCoder rename it"
	interrupts := 0
	for _, m := range c.all()[before:] {
		if m.Type == "interrupt" {
			interrupts++
		}
	}
	if interrupts != 1 {
		t.Errorf("interrupts = %d, want 1", interrupts)
	}
	select {
	case prompt := <-stub.started:
		if prompt != "rename it" {
			t.Errorf("request = %q, want %q", prompt, "rename it")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no run after hey VoiceCoder: %+v", c.all())
	}
	close(stub.release)
}
