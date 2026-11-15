package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theatlantadream/voicecoder-bridge/internal/chat"
)

type stubChatter struct {
	mu      sync.Mutex
	systems []string
	msgs    [][]chat.Message
	reply   func(system string, msgs []chat.Message) (string, error)
}

func (c *stubChatter) Reply(ctx context.Context, system string, msgs []chat.Message) (string, error) {
	c.mu.Lock()
	c.systems = append(c.systems, system)
	c.msgs = append(c.msgs, msgs)
	c.mu.Unlock()
	return c.reply(system, msgs)
}

func (c *stubChatter) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.systems)
}

func fixed(text string) func(string, []chat.Message) (string, error) {
	return func(string, []chat.Message) (string, error) { return text, nil }
}

func speeches(c *collector, kind string) []outbound {
	var out []outbound
	for _, m := range c.all() {
		if m.Type == "speech" && m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

func waitSpeech(t *testing.T, c *collector, kind string) outbound {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := speeches(c, kind); len(got) > 0 {
			return got[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s speech: %+v", kind, c.all())
	return outbound{}
}

func TestParseAction(t *testing.T) {
	cases := []struct{ in, action, text string }{
		{"ACTION: none\nSure thing.", "none", "Sure thing."},
		{"action: CANCEL\n\nOkay, stopped.", "cancel", "Okay, stopped."},
		{"ACTION: queue\nI'll do that next.", "queue", "I'll do that next."},
		{"Just words.", "none", "Just words."},
		{"ACTION: delete\nNo.", "none", "ACTION: delete\nNo."},
		{"ACTION: cancel", "cancel", ""},
	}
	for _, c := range cases {
		if a, text := parseAction(c.in); a != c.action || text != c.text {
			t.Errorf("parseAction(%q) = %q, %q", c.in, a, text)
		}
	}
}

// TestHeyCoderTalksWithoutStopping answers the user mid-run and leaves the
// run going.
func TestHeyCoderTalksWithoutStopping(t *testing.T) {
	stub := newStubAgent()
	profile := filepath.Join(t.TempDir(), "profile.md")
	if err := os.WriteFile(profile, []byte("Name: Sam\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ch := &stubChatter{reply: fixed("ACTION: none\nGoing well, I'm about `halfway`.")}
	sp := &stubSpeaker{audio: []byte("mp3")}
	tr := &fakeTranscriber{lines: []string{"Hey, Coder", "how is it going over"}}
	srv := NewServer(tr, WithAgent(stub), WithEndWord("over"), WithSpeaker(sp), WithChatter(ch, profile))
	srv.narrateFirst = time.Hour
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	waitStarted(t, stub)

	hear(srv, sess, c, 0)
	c.waitFor(t, "interrupt")
	hear(srv, sess, c, 1)
	srv.finishUtterance(context.Background(), sess, c.send)

	got := waitSpeech(t, c, "reply")
	if got.Text != "Going well, I'm about halfway." {
		t.Errorf("reply = %q", got.Text)
	}
	if len(stub.stopped) != 0 || !agentRunning(sess) {
		t.Fatal("the run was stopped by a side question")
	}
	ch.mu.Lock()
	system, msgs := ch.systems[0], ch.msgs[0]
	ch.mu.Unlock()
	if !strings.Contains(system, "add a function") || !strings.Contains(system, "Name: Sam") {
		t.Errorf("system prompt lacks the request or profile: %q", system)
	}
	if len(msgs) != 1 || msgs[0].Content != "how is it going" {
		t.Errorf("msgs = %+v", msgs)
	}
	close(stub.release)
	c.waitFor(t, "agent_result")
}

// TestSideTalkCancel stops the run when the model says the user wants that.
func TestSideTalkCancel(t *testing.T) {
	stub := newStubAgent()
	ch := &stubChatter{reply: fixed("ACTION: cancel\nOkay, I've stopped.")}
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithSpeaker(&stubSpeaker{audio: []byte("mp3")}), WithChatter(ch, ""))
	srv.narrateFirst = time.Hour
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	waitStarted(t, stub)
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "actually forget that one"})
	select {
	case <-stub.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("run not stopped")
	}
	waitSpeech(t, c, "reply")
}

// TestSideTalkQueuesNextRequest starts a request given mid-run once the
// current one is done.
func TestSideTalkQueuesNextRequest(t *testing.T) {
	stub := newStubAgent()
	ch := &stubChatter{reply: fixed("ACTION: queue\nI'll do that right after this.")}
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithChatter(ch, ""))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	waitStarted(t, stub)
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "also add a test"})
	deadline := time.Now().Add(2 * time.Second)
	for !c.has("status", "I'll do that right after this.") {
		if time.Now().After(deadline) {
			t.Fatalf("no reply: %+v", c.all())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(stub.started) != 0 {
		t.Fatal("queued request started early")
	}
	stub.release <- struct{}{}
	select {
	case prompt := <-stub.started:
		if prompt != "also add a test" {
			t.Errorf("queued prompt = %q", prompt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("queued request never started: %+v", c.all())
	}
	close(stub.release)
}

// TestStopDropsQueuedRequest: the stop key cancels everything.
func TestStopDropsQueuedRequest(t *testing.T) {
	stub := newStubAgent()
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithChatter(&stubChatter{reply: fixed("ACTION: queue\nNext.")}, ""))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "one"})
	waitStarted(t, stub)
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "two"})
	deadline := time.Now().Add(2 * time.Second)
	for !c.has("status", "Next.") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	srv.handleControl(context.Background(), sess, c.send, control{Type: "stop"})
	c.waitFor(t, "error")
	time.Sleep(50 * time.Millisecond)
	if len(stub.started) != 0 {
		t.Fatal("queued request ran after stop")
	}
}

// TestSideTalkFailureKeepsRun falls back to the plain busy message.
func TestSideTalkFailureKeepsRun(t *testing.T) {
	stub := newStubAgent()
	ch := &stubChatter{reply: func(string, []chat.Message) (string, error) { return "", errors.New("boom") }}
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithChatter(ch, ""))
	sess := &session{partials: make(map[int]string)}
	c := &collector{}
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "one"})
	waitStarted(t, stub)
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "hello?"})
	deadline := time.Now().Add(2 * time.Second)
	for !c.has("status", stillWorking) {
		if time.Now().After(deadline) {
			t.Fatalf("no fallback: %+v", c.all())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !agentRunning(sess) {
		t.Fatal("run stopped")
	}
	close(stub.release)
}

// TestNarrationWhileWorking speaks plain lines during a run, never the
// agent's raw notes, and skips when nothing changed.
func TestNarrationWhileWorking(t *testing.T) {
	stub := newStubAgent()
	stub.stream = true
	ch := &stubChatter{reply: fixed("I'm setting up the new function now.\n```go\nfunc secret() {}\n```")}
	sp := &stubSpeaker{audio: []byte("mp3")}
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithSpeaker(sp), WithChatter(ch, ""))
	srv.narrateFirst, srv.narrateEvery = 10*time.Millisecond, 10*time.Millisecond
	sess := &session{partials: make(map[int]string)}
	c := &collector{}

	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	waitStarted(t, stub)
	got := waitSpeech(t, c, "narration")
	if got.Text != "I'm setting up the new function now." {
		t.Errorf("narration = %q", got.Text)
	}
	time.Sleep(80 * time.Millisecond) // more ticks, same notes: nothing new to say
	if n := ch.calls(); n != 1 {
		t.Errorf("chatter calls = %d, want 1 while the notes are unchanged", n)
	}
	ch.mu.Lock()
	system := ch.systems[0]
	ch.mu.Unlock()
	if !strings.Contains(system, "never say code") {
		t.Error("narration prompt lost the no-code rule")
	}
	close(stub.release)
	c.waitFor(t, "agent_result")
	before := len(speeches(c, "narration"))
	time.Sleep(50 * time.Millisecond)
	if len(speeches(c, "narration")) != before {
		t.Error("narration after the run ended")
	}
}

func TestNarrationSkip(t *testing.T) {
	stub := newStubAgent()
	ch := &stubChatter{reply: fixed("SKIP")}
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithSpeaker(&stubSpeaker{audio: []byte("mp3")}), WithChatter(ch, ""))
	srv.narrateFirst, srv.narrateEvery = 10*time.Millisecond, 10*time.Millisecond
	sess := &session{partials: make(map[int]string)}
	c := &collector{}
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	waitStarted(t, stub)
	time.Sleep(80 * time.Millisecond)
	if len(speeches(c, "narration")) != 0 {
		t.Fatal("SKIP was spoken")
	}
	close(stub.release)
}

// TestSideTalkRemembers saves what the user says about themselves to the
// local profile, and never speaks the REMEMBER line.
func TestSideTalkRemembers(t *testing.T) {
	stub := newStubAgent()
	profile := filepath.Join(t.TempDir(), ".voicecoder", "profile.md")
	ch := &stubChatter{reply: fixed("ACTION: none\nREMEMBER: The user's name is Sam and they are new to coding.\nNice to meet you, Sam!")}
	srv := NewServer(&fakeTranscriber{}, WithAgent(stub), WithSpeaker(&stubSpeaker{audio: []byte("mp3")}), WithChatter(ch, profile))
	srv.narrateFirst = time.Hour
	sess := &session{partials: make(map[int]string)}
	c := &collector{}
	srv.handleControl(context.Background(), sess, c.send, control{Type: "text", Text: "add a function"})
	waitStarted(t, stub)
	for i := 0; i < 2; i++ { // said twice, stored once
		srv.sideTalk(context.Background(), sess, c.send, "I'm Sam, I'm new to this")
	}
	if got := waitSpeech(t, c, "reply"); got.Text != "Nice to meet you, Sam!" {
		t.Errorf("reply = %q", got.Text)
	}
	raw, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "- The user's name is Sam and they are new to coding.\n" {
		t.Errorf("profile = %q", raw)
	}
	close(stub.release)
}
