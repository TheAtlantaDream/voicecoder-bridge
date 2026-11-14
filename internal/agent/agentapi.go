package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// interruptKey is written to the agent's terminal to stop a running task.
// Esc is what Claude Code is expected to use; change it here if a live
// check shows another key.
const interruptKey = "\u001b"

// wrapWidth is the terminal width AgentAPI formats agent messages to.
const wrapWidth = 80

var (
	errBusy        = errors.New("The agent is still busy. Say stop or wait a moment.")
	errStopped     = errors.New("Stopped.")
	errTimeout     = errors.New("That took too long, try a smaller request.")
	errUnreachable = errors.New("I can't reach the coding agent right now.")
	errRejected    = errors.New("The coding agent did not accept that request.")
	errNoDiff      = errors.New("The agent finished, but I could not read the changes in the project folder.")
)

const noSummary = "Done, but the agent gave no summary."

type AgentAPI struct {
	baseURL string
	repo    string
	timeout time.Duration
	poll    time.Duration
	client  *http.Client
}

func NewAgentAPI(baseURL, repo string, timeout time.Duration) *AgentAPI {
	return &AgentAPI{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		repo:    repo,
		timeout: timeout,
		poll:    500 * time.Millisecond,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (a *AgentAPI) Run(ctx context.Context, prompt string) (Result, error) {
	runCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	sent := false
	fail := func(err error) (Result, error) {
		if runCtx.Err() == nil {
			return Result{}, err
		}
		if sent {
			a.interrupt()
		}
		if ctx.Err() != nil {
			return Result{}, errStopped
		}
		return Result{}, errTimeout
	}

	status, err := a.status(runCtx)
	if err != nil {
		return fail(errUnreachable)
	}
	if status != "stable" {
		return fail(errBusy)
	}
	before, err := a.messages(runCtx)
	if err != nil {
		return fail(errUnreachable)
	}

	sent = true
	code, err := a.post(runCtx, "user", prompt)
	if err != nil {
		return fail(errUnreachable)
	}
	if code != http.StatusOK {
		sent = false
		return fail(errRejected)
	}

	tick := time.NewTicker(a.poll)
	defer tick.Stop()
	for {
		select {
		case <-runCtx.Done():
			return fail(runCtx.Err())
		case <-tick.C:
		}
		status, err := a.status(runCtx)
		if err != nil {
			return fail(errUnreachable)
		}
		if status == "stable" {
			break
		}
	}

	after, err := a.messages(runCtx)
	if err != nil {
		return fail(errUnreachable)
	}
	summary := ""
	for i := len(after) - 1; i >= len(before); i-- {
		if after[i].Role == "agent" {
			summary = unwrap(after[i].Content)
			break
		}
	}
	if summary == "" {
		summary = noSummary
	}

	diff, err := Diff(runCtx, a.repo)
	if err != nil {
		return fail(errNoDiff)
	}
	return Result{Summary: summary, Diff: diff}, nil
}

func (a *AgentAPI) status(ctx context.Context) (string, error) {
	var body struct {
		Status string `json:"status"`
	}
	if err := a.get(ctx, "/status", &body); err != nil {
		return "", err
	}
	return body.Status, nil
}

func (a *AgentAPI) messages(ctx context.Context) ([]apiMessage, error) {
	var body struct {
		Messages []apiMessage `json:"messages"`
	}
	if err := a.get(ctx, "/messages", &body); err != nil {
		return nil, err
	}
	return body.Messages, nil
}

func (a *AgentAPI) get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agentapi %s returned %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func (a *AgentAPI) post(ctx context.Context, msgType, content string) (int, error) {
	payload, err := json.Marshal(map[string]string{"type": msgType, "content": content})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/message", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// interrupt runs on its own short context because the run's context is
// already done by the time it is needed.
func (a *AgentAPI) interrupt() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = a.post(ctx, "raw", interruptKey)
}

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	listRe = regexp.MustCompile(`^([-*•+]|\d+[.)])\s`)
)

// unwrap turns a terminal-formatted message back into normal sentences. A
// line is joined to the one before it only when its first word would not
// have fit on that line, so real line breaks, list items, and paragraphs
// stay as they are.
func unwrap(text string) string {
	text = ansiRe.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "\r", "")

	var out []string
	prevLen := 0
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimRight(raw, " \t")
		line := strings.TrimSpace(raw)
		if line == "" {
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			prevLen = 0
			continue
		}
		first := line
		if i := strings.IndexAny(line, " \t"); i > 0 {
			first = line[:i]
		}
		wrapped := prevLen > 0 && prevLen+1+len([]rune(first)) > wrapWidth && !listRe.MatchString(line)
		if wrapped {
			out[len(out)-1] += " " + line
		} else {
			out = append(out, line)
		}
		prevLen = len([]rune(raw))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
