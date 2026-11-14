package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// UpdateFunc receives the latest agent reply text while a run is in progress.
// Implementations may call it zero or more times. nil is allowed.
type UpdateFunc func(text string)

type Agent interface {
	Run(ctx context.Context, prompt string, onUpdate UpdateFunc) (Result, error)
}

// Result is what comes back from one request: Summary is read aloud,
// Diff is shown on screen.
type Result struct {
	Summary string
	Diff    string
}

// FakeLabel starts every fake summary so a fake result can never look real.
const FakeLabel = "[FAKE AGENT]"

const fakeDiff = `diff --git a/example.txt b/example.txt
--- a/example.txt
+++ b/example.txt
@@ -1 +1 @@
-before
+after
`

type Fake struct {
	Delay time.Duration
}

func (f Fake) Run(ctx context.Context, prompt string, onUpdate UpdateFunc) (Result, error) {
	summary := FakeLabel + " No code was changed. The request was: " + prompt
	if onUpdate != nil {
		onUpdate(FakeLabel + " Working on: " + prompt)
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(f.Delay / 2):
		}
		onUpdate(summary)
	}
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-time.After(f.Delay / 2):
	}
	res := Result{Summary: summary, Diff: fakeDiff}
	return res, nil
}

// emptyTree is git's well-known empty tree, the base for a repo with no commits.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Diff returns every change in repo that is not committed yet: staged and
// unstaged edits to tracked files, plus untracked files that are not ignored,
// shown as added. It is empty when there is nothing. It only reads.
func Diff(ctx context.Context, repo string) (string, error) {
	base := "HEAD"
	if _, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if _, err := git(ctx, repo, "rev-parse", "--git-dir"); err != nil {
			return "", err
		}
		base = emptyTree
	}

	diff, err := git(ctx, repo, "diff", "--no-color", "--no-ext-diff", base)
	if err != nil {
		return "", err
	}

	untracked, err := git(ctx, repo, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	for _, name := range strings.Split(untracked, "\x00") {
		if name == "" {
			continue
		}
		// --no-index exits 1 when the files differ, which is always the case here.
		added, err := git(ctx, repo, "diff", "--no-color", "--no-ext-diff", "--no-index", "--", "/dev/null", name)
		var exit *exec.ExitError
		if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
			return "", err
		}
		diff += added
	}
	return diff, nil
}

func git(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}
