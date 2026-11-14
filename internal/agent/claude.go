package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os/exec"
	"strings"
	"time"
)

var errClaudeFailed = errors.New("The coding agent ran into a problem and did not finish.")

// Claude runs the claude command once per request, without AgentAPI. It is
// the fallback for when AgentAPI misbehaves.
type Claude struct {
	repo    string
	timeout time.Duration
}

// NewClaude returns an agent that runs claude in repo. timeout is the
// longest one request may run.
func NewClaude(repo string, timeout time.Duration) *Claude {
	return &Claude{repo: repo, timeout: timeout}
}

// claudeOutput is the part of `claude -p --output-format json` we read.
type claudeOutput struct {
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

func (c *Claude) Run(ctx context.Context, prompt string, onUpdate UpdateFunc) (Result, error) {
	runCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	fail := func(err error) (Result, error) {
		if ctx.Err() != nil {
			return Result{}, errStopped
		}
		if runCtx.Err() != nil {
			return Result{}, errTimeout
		}
		return Result{}, err
	}

	baseline, err := SnapshotTree(runCtx, c.repo)
	if err != nil {
		log.Printf("claude: snapshot before run: %v", err)
		return fail(errNoDiff)
	}

	cmd := exec.CommandContext(runCtx, "claude", "-p", prompt, "--output-format", "json")
	cmd.Dir = c.repo
	// Do not wait forever on output pipes held open by leftover child processes.
	cmd.WaitDelay = 2 * time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) && runCtx.Err() == nil {
			return fail(errUnreachable) // claude is not installed, or repo is missing
		}
		return fail(errClaudeFailed)
	}

	var out claudeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.IsError {
		return fail(errClaudeFailed)
	}
	summary := strings.TrimSpace(out.Result)
	if summary == "" {
		summary = noSummary
	}
	if onUpdate != nil {
		onUpdate(summary)
	}

	diff, err := DiffSince(runCtx, c.repo, baseline)
	if err != nil {
		log.Printf("claude: diff since snapshot: %v", err)
		return fail(errNoDiff)
	}
	return Result{Summary: summary, Diff: diff}, nil
}
