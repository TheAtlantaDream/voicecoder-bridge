package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var _ Agent = (*AgentAPI)(nil)

// fakeAgentAPI stands in for a running AgentAPI server. After a user
// message it reports "running" for pollsUntilStable status checks, then
// goes back to "stable" and adds reply as the agent's message.
type fakeAgentAPI struct {
	mu               sync.Mutex
	status           string
	messages         []apiMessage
	reply            string
	pollsUntilStable int // negative: never finishes
	pollsLeft        int
	onRunning        func()
	user             []string
	raw              []string
}

func (f *fakeAgentAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/status":
		if f.status == "running" && f.pollsUntilStable >= 0 {
			if f.pollsLeft == 0 {
				f.status = "stable"
				if f.reply != "" {
					f.messages = append(f.messages, apiMessage{Role: "agent", Content: f.reply})
				}
			} else {
				f.pollsLeft--
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": f.status})
	case "/messages":
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": f.messages})
	case "/message":
		var m struct{ Type, Content string }
		_ = json.NewDecoder(r.Body).Decode(&m)
		if m.Type == "raw" {
			f.raw = append(f.raw, m.Content)
		} else {
			f.user = append(f.user, m.Content)
			f.messages = append(f.messages, apiMessage{Role: "user", Content: m.Content})
			f.status = "running"
			f.pollsLeft = f.pollsUntilStable
			if f.onRunning != nil {
				go f.onRunning()
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAgentAPI) rawSent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.raw...)
}

func (f *fakeAgentAPI) userSent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.user...)
}

// newClient starts the fake server and returns a client that polls fast.
func newClient(t *testing.T, f *fakeAgentAPI, repo string, timeout time.Duration) *AgentAPI {
	t.Helper()
	if f.status == "" {
		f.status = "stable"
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	a := NewAgentAPI(srv.URL, repo, timeout)
	a.poll = 5 * time.Millisecond
	return a
}

// TestAgentAPIHappyPath checks the whole run: the prompt is sent as a user
// message, and the result is the agent's last new reply, un-wrapped, plus
// the diff of the project folder.
func TestAgentAPIHappyPath(t *testing.T) {
	repo := newRepo(t, true)
	write(t, repo, "tracked.txt", "two\n")
	f := &fakeAgentAPI{
		messages:         []apiMessage{{Role: "agent", Content: "Welcome, this is an old message."}},
		reply:            "I changed tracked.txt so that it now says two instead of one, and there are no\ntests in this project to run.",
		pollsUntilStable: 2,
	}
	a := newClient(t, f, repo, time.Minute)

	res, err := a.Run(context.Background(), "change one to two")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "I changed tracked.txt so that it now says two instead of one, and there are no tests in this project to run."
	if res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if !strings.Contains(res.Diff, "+two") {
		t.Errorf("diff = %q, want the change to tracked.txt", res.Diff)
	}
	if got := f.userSent(); len(got) != 1 || got[0] != "change one to two" {
		t.Errorf("user messages sent = %q, want the prompt once", got)
	}
	if got := f.rawSent(); len(got) != 0 {
		t.Errorf("raw messages sent = %q, want none on a normal run", got)
	}
}

// TestAgentAPIBusy checks that a request is refused, and nothing is sent,
// while the agent is still working on something else.
func TestAgentAPIBusy(t *testing.T) {
	f := &fakeAgentAPI{status: "running", pollsUntilStable: -1}
	a := newClient(t, f, newRepo(t, true), time.Minute)

	_, err := a.Run(context.Background(), "anything")
	if err != errBusy {
		t.Errorf("err = %v, want the busy error", err)
	}
	if len(f.userSent()) != 0 || len(f.rawSent()) != 0 {
		t.Errorf("sent messages to a busy agent, want none")
	}
}

// TestAgentAPITimeout checks that a run that never finishes is interrupted
// with Esc and reported as taking too long.
func TestAgentAPITimeout(t *testing.T) {
	f := &fakeAgentAPI{pollsUntilStable: -1}
	a := newClient(t, f, newRepo(t, true), 50*time.Millisecond)

	_, err := a.Run(context.Background(), "a huge request")
	if err != errTimeout {
		t.Errorf("err = %v, want the timeout error", err)
	}
	if got := f.rawSent(); len(got) != 1 || got[0] != "\x1b" {
		t.Errorf("raw messages sent = %q, want one Esc", got)
	}
}

// TestAgentAPICancel checks that cancelling the context mid-run interrupts
// the agent with Esc and reports the run as stopped.
func TestAgentAPICancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeAgentAPI{pollsUntilStable: -1, onRunning: cancel}
	a := newClient(t, f, newRepo(t, true), time.Minute)

	_, err := a.Run(ctx, "something slow")
	if err != errStopped {
		t.Errorf("err = %v, want the stopped error", err)
	}
	if got := f.rawSent(); len(got) != 1 || got[0] != "\x1b" {
		t.Errorf("raw messages sent = %q, want one Esc", got)
	}
}

// TestAgentAPINoNewMessage checks that a run where the agent says nothing
// still gives the user a fixed line to hear, not an old message.
func TestAgentAPINoNewMessage(t *testing.T) {
	f := &fakeAgentAPI{
		messages: []apiMessage{{Role: "agent", Content: "An old reply."}},
	}
	a := newClient(t, f, newRepo(t, true), time.Minute)

	res, err := a.Run(context.Background(), "do nothing")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != noSummary {
		t.Errorf("summary = %q, want %q", res.Summary, noSummary)
	}
}

// TestAgentAPIDown checks that an unreachable AgentAPI gives a plain error
// the user can hear, not a network error.
func TestAgentAPIDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	_, err := NewAgentAPI(url, t.TempDir(), time.Minute).Run(context.Background(), "anything")
	if err != errUnreachable {
		t.Errorf("err = %v, want the unreachable error", err)
	}
}

// TestUnwrap checks that terminal-wrapped lines become normal sentences
// while real line breaks, lists, and paragraphs are kept.
func TestUnwrap(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			"wrapped sentence",
			"I added a function called add to math_utils.py and a test for it in the file\ntest_math_utils.py. All 3 tests pass.",
			"I added a function called add to math_utils.py and a test for it in the file test_math_utils.py. All 3 tests pass.",
		},
		{
			"short lines stay apart",
			"Done.\nTests pass.",
			"Done.\nTests pass.",
		},
		{
			"paragraphs kept",
			"First paragraph.\n\n\nSecond paragraph.",
			"First paragraph.\n\nSecond paragraph.",
		},
		{
			"list items kept",
			"Changed files:\n- math_utils.py now has an add function that takes two numbers and returns it\n- test_math_utils.py is new",
			"Changed files:\n- math_utils.py now has an add function that takes two numbers and returns it\n- test_math_utils.py is new",
		},
		{
			"wrapped list item",
			"- math_utils.py now has an add function that takes two numbers and returns the\n  sum of both\n- test_math_utils.py is new",
			"- math_utils.py now has an add function that takes two numbers and returns the sum of both\n- test_math_utils.py is new",
		},
		{
			"terminal artifacts",
			"\x1b[1mI fixed the bug.\x1b[0m   \r\n\r\n  The test passes.  \n\n",
			"I fixed the bug.\n\nThe test passes.",
		},
		{
			"empty",
			"  \n \n",
			"",
		},
	}
	for _, c := range cases {
		if got := unwrap(c.in); got != c.want {
			t.Errorf("%s: unwrap = %q, want %q", c.name, got, c.want)
		}
	}
}
