package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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

// SnapshotTree records the full working tree (HEAD + staged + unstaged +
// untracked, excluding ignored files) as a git tree object, without changing
// the real index. Used as the baseline before an agent run.
func SnapshotTree(ctx context.Context, repo string) (string, error) {
	if _, err := git(ctx, repo, "rev-parse", "--git-dir"); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "voicecoder-index-*")
	if err != nil {
		return "", err
	}
	indexPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(indexPath)

	env := []string{"GIT_INDEX_FILE=" + indexPath}
	if _, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		if _, err := gitEnv(ctx, repo, env, "read-tree", "HEAD"); err != nil {
			return "", err
		}
	} else if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if _, err := gitEnv(ctx, repo, env, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := gitEnv(ctx, repo, env, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(tree), nil
}

// DiffSince returns the unified diff of the working tree against a tree from
// SnapshotTree. Empty when nothing changed since the snapshot.
func DiffSince(ctx context.Context, repo, before string) (string, error) {
	after, err := SnapshotTree(ctx, repo)
	if err != nil {
		return "", err
	}
	if before == after {
		return "", nil
	}
	return gitDiff(ctx, repo, before, after)
}

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

	diff, err := gitDiff(ctx, repo, base)
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
		added, err := gitDiffAllowExit1(ctx, repo, nil, "--no-color", "--no-ext-diff", "--no-index", "--", "/dev/null", name)
		if err != nil {
			return "", err
		}
		diff += added
	}
	return diff, nil
}

func gitDiff(ctx context.Context, repo string, revs ...string) (string, error) {
	args := append([]string{"--no-color", "--no-ext-diff"}, revs...)
	return gitDiffAllowExit1(ctx, repo, nil, args...)
}

func gitDiffAllowExit1(ctx context.Context, repo string, env []string, diffArgs ...string) (string, error) {
	args := append([]string{"diff"}, diffArgs...)
	out, err := gitEnv(ctx, repo, env, args...)
	if err == nil {
		return out, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return out, nil
	}
	return "", err
}

func git(ctx context.Context, repo string, args ...string) (string, error) {
	return gitEnv(ctx, repo, nil, args...)
}

func gitEnv(ctx context.Context, repo string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}
