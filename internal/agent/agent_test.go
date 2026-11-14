package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var _ Agent = Fake{}

// TestFakeIsLabeled checks that a fake result can never be mistaken for a
// real one: its summary always starts with the fake label.
func TestFakeIsLabeled(t *testing.T) {
	res, err := Fake{}.Run(context.Background(), "fix the login bug")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(res.Summary, FakeLabel) {
		t.Errorf("summary = %q, want it to start with %q", res.Summary, FakeLabel)
	}
	if res.Diff == "" {
		t.Errorf("diff is empty, want a sample diff")
	}
}

// TestFakeStopsOnCancel checks that a fake run ends as soon as its context
// is cancelled instead of waiting out the delay.
func TestFakeStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Fake{Delay: time.Minute}.Run(ctx, "anything")
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake run did not stop after cancel")
	}
}

// newRepo makes a temp git repo. With commit set, it has one committed file.
func newRepo(t *testing.T, commit bool) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	run(t, dir, "config", "user.name", "Test")
	run(t, dir, "config", "user.email", "test@example.com")
	if commit {
		write(t, dir, "tracked.txt", "one\n")
		run(t, dir, "add", "-A")
		run(t, dir, "commit", "-q", "-m", "first")
	}
	return dir
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func diffOf(t *testing.T, dir string) string {
	t.Helper()
	d, err := Diff(context.Background(), dir)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	return d
}

// TestDiffCleanRepo checks that a repo with nothing unapproved gives an
// empty diff.
func TestDiffCleanRepo(t *testing.T) {
	if d := diffOf(t, newRepo(t, true)); d != "" {
		t.Errorf("diff = %q, want empty for a clean repo", d)
	}
}

// TestDiffTrackedChanges checks that edits to tracked files show up
// whether or not they are staged.
func TestDiffTrackedChanges(t *testing.T) {
	for _, stage := range []bool{false, true} {
		dir := newRepo(t, true)
		write(t, dir, "tracked.txt", "two\n")
		if stage {
			run(t, dir, "add", "-A")
		}
		d := diffOf(t, dir)
		if !strings.Contains(d, "-one") || !strings.Contains(d, "+two") {
			t.Errorf("staged=%v: diff = %q, want the change from one to two", stage, d)
		}
	}
}

// TestDiffUntrackedFile checks that a new file the agent created is shown
// as an added file, next to changes to tracked files.
func TestDiffUntrackedFile(t *testing.T) {
	dir := newRepo(t, true)
	write(t, dir, "tracked.txt", "two\n")
	write(t, dir, "new.txt", "hello\n")
	d := diffOf(t, dir)
	for _, want := range []string{"+two", "new.txt", "new file mode", "+hello"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff is missing %q:\n%s", want, d)
		}
	}
}

// TestDiffNoCommits checks that a freshly initialised repo with no commits
// still gives a diff, for staged and untracked files alike.
func TestDiffNoCommits(t *testing.T) {
	dir := newRepo(t, false)
	if d := diffOf(t, dir); d != "" {
		t.Errorf("diff = %q, want empty for an empty repo", d)
	}
	write(t, dir, "staged.txt", "staged\n")
	run(t, dir, "add", "-A")
	write(t, dir, "loose.txt", "loose\n")
	d := diffOf(t, dir)
	for _, want := range []string{"+staged", "+loose"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff is missing %q:\n%s", want, d)
		}
	}
}

// TestDiffSkipsIgnoredFiles checks that files matched by .gitignore never
// appear in the diff.
func TestDiffSkipsIgnoredFiles(t *testing.T) {
	dir := newRepo(t, true)
	write(t, dir, ".gitignore", "secret.txt\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "ignore")
	write(t, dir, "secret.txt", "do not show\n")
	if d := diffOf(t, dir); d != "" {
		t.Errorf("diff = %q, want empty: the only change is an ignored file", d)
	}
}

// TestDiffNotARepo checks that a folder that is not a git repo is an error,
// not an empty diff.
func TestDiffNotARepo(t *testing.T) {
	if _, err := Diff(context.Background(), t.TempDir()); err == nil {
		t.Error("err = nil, want an error for a folder that is not a repo")
	}
}
