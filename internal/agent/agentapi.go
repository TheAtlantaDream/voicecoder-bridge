package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		// No client Timeout: POST /message blocks until the agent starts
		// processing (AgentAPI may wait >10s for the PTY to stabilize).
		// Request contexts carry the real deadline (run timeout / interrupt).
		client: &http.Client{},
	}
}

type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (a *AgentAPI) Run(ctx context.Context, prompt string, onUpdate UpdateFunc) (Result, error) {
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
	beforeLen := len(before)

	sent = true
	code, err := a.post(runCtx, "user", prompt)
	if err != nil {
		return fail(errUnreachable)
	}
	if code != http.StatusOK {
		sent = false
		return fail(errRejected)
	}

	lastPartial := ""
	emit := func(text string) {
		text = unwrap(text)
		if text == "" || text == lastPartial || onUpdate == nil {
			return
		}
		lastPartial = text
		onUpdate(text)
	}

	// Prefer SSE for live updates; fall back to polling /messages.
	useSSE := a.streamEvents(runCtx, beforeLen, emit)
	if !useSSE {
		tick := time.NewTicker(a.poll)
		defer tick.Stop()
		for {
			select {
			case <-runCtx.Done():
				return fail(runCtx.Err())
			case <-tick.C:
			}
			a.emitLatestAgent(runCtx, beforeLen, emit)
			status, err := a.status(runCtx)
			if err != nil {
				return fail(errUnreachable)
			}
			if status == "stable" {
				break
			}
		}
	} else {
		// SSE ended because status went stable or the stream closed; confirm.
		for {
			status, err := a.status(runCtx)
			if err != nil {
				return fail(errUnreachable)
			}
			if status == "stable" {
				break
			}
			select {
			case <-runCtx.Done():
				return fail(runCtx.Err())
			case <-time.After(a.poll):
			}
			a.emitLatestAgent(runCtx, beforeLen, emit)
		}
	}

	after, err := a.messages(runCtx)
	if err != nil {
		return fail(errUnreachable)
	}
	summary := ""
	for i := len(after) - 1; i >= beforeLen; i-- {
		if after[i].Role == "agent" {
			summary = unwrap(after[i].Content)
			break
		}
	}
	if summary == "" {
		summary = noSummary
	}
	if onUpdate != nil && summary != lastPartial {
		onUpdate(summary)
	}

	diff, err := Diff(runCtx, a.repo)
	if err != nil {
		return fail(errNoDiff)
	}
	return Result{Summary: summary, Diff: diff}, nil
}

func (a *AgentAPI) emitLatestAgent(ctx context.Context, beforeLen int, emit func(string)) {
	msgs, err := a.messages(ctx)
	if err != nil {
		return
	}
	for i := len(msgs) - 1; i >= beforeLen; i-- {
		if msgs[i].Role == "agent" {
			emit(msgs[i].Content)
			return
		}
	}
}

// streamEvents reads GET /events. It returns true if the stream was used
// (even briefly). false means the caller should poll instead.
func (a *AgentAPI) streamEvents(ctx context.Context, beforeLen int, emit func(string)) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/events", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := a.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	reader := bufio.NewReader(resp.Body)
	var dataLines []string
	sawEvent := false
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			sawEvent = true
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if len(dataLines) > 0 {
					payload := strings.Join(dataLines, "\n")
					dataLines = dataLines[:0]
					a.handleSSEData(ctx, payload, beforeLen, emit)
					if st, stErr := a.status(ctx); stErr == nil && st == "stable" {
						return true
					}
				}
			case strings.HasPrefix(trimmed, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return sawEvent
			}
			return sawEvent
		}
	}
}

func (a *AgentAPI) handleSSEData(ctx context.Context, payload string, beforeLen int, emit func(string)) {
	var evt struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Role    string `json:"role"`
		Content string `json:"content"`
		Message *struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		// Some AgentAPI builds send bare message objects; try that.
		var msg apiMessage
		if json.Unmarshal([]byte(payload), &msg) == nil && msg.Role == "agent" && msg.Content != "" {
			emit(msg.Content)
		} else {
			a.emitLatestAgent(ctx, beforeLen, emit)
		}
		return
	}
	switch {
	case evt.Message != nil && evt.Message.Role == "agent" && evt.Message.Content != "":
		emit(evt.Message.Content)
	case evt.Role == "agent" && evt.Content != "":
		emit(evt.Content)
	case evt.Type == "message_update" || evt.Type == "agent_message" || evt.Content != "":
		if evt.Content != "" {
			emit(evt.Content)
		} else {
			a.emitLatestAgent(ctx, beforeLen, emit)
		}
	default:
		a.emitLatestAgent(ctx, beforeLen, emit)
	}
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
