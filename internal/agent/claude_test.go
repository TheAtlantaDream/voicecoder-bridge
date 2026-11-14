package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var _ Agent = (*Claude)(nil)

// stubClaude puts a shell script named claude first on PATH, standing in
// for the real command.
func stubClaude(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestClaudeHappyPath(t *testing.T) {
	repo := newRepo(t, true)
	// The stub records its arguments and folder, edits a file, and prints
	// the JSON the real command prints.
	stubClaude(t, `printf '%s\n' "$@" > args.txt
pwd > cwd.txt
echo "package a" > new.go
printf '%s\n' '{"type":"result","is_error":false,"result":" I added new.go. \n"}'`)

	res, err := NewClaude(repo, 5*time.Second).Run(context.Background(), "add a file", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "I added new.go." {
		t.Errorf("Summary = %q, want the result field, trimmed", res.Summary)
	}
	if !strings.Contains(res.Diff, "new.go") {
		t.Errorf("Diff = %q, want it to show new.go", res.Diff)
	}

	args, _ := os.ReadFile(filepath.Join(repo, "args.txt"))
	if got, want := string(args), "-p\nadd a file\n--output-format\njson\n"; got != want {
		t.Errorf("claude args = %q, want %q", got, want)
	}
	cwd, _ := os.ReadFile(filepath.Join(repo, "cwd.txt"))
	want, _ := filepath.EvalSymlinks(repo)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(cwd)))
	if got != want {
		t.Errorf("claude ran in %q, want the project folder %q", got, want)
	}
}

func TestClaudeNoSummary(t *testing.T) {
	stubClaude(t, `echo '{"type":"result","is_error":false,"result":""}'`)
	res, err := NewClaude(newRepo(t, true), 5*time.Second).Run(context.Background(), "do nothing", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != noSummary {
		t.Errorf("Summary = %q, want %q", res.Summary, noSummary)
	}
}

func TestClaudeFailures(t *testing.T) {
	cases := []struct {
		name   string
		script string
	}{
		{"non-zero exit", `echo boom >&2; exit 1`},
		{"is_error result", `echo '{"type":"result","is_error":true,"result":"Invalid API key"}'`},
		{"output is not JSON", `echo "not json"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubClaude(t, tc.script)
			_, err := NewClaude(newRepo(t, true), 5*time.Second).Run(context.Background(), "x", nil)
			if err != errClaudeFailed {
				t.Errorf("err = %v, want %v", err, errClaudeFailed)
			}
		})
	}
}

func TestClaudeTimeout(t *testing.T) {
	stubClaude(t, `exec sleep 30`)
	start := time.Now()
	_, err := NewClaude(newRepo(t, true), 100*time.Millisecond).Run(context.Background(), "x", nil)
	if err != errTimeout {
		t.Errorf("err = %v, want %v", err, errTimeout)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Run took %s, want claude killed at the timeout", took)
	}
}

func TestClaudeCancel(t *testing.T) {
	stubClaude(t, `exec sleep 30`)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := NewClaude(newRepo(t, true), 30*time.Second).Run(ctx, "x", nil)
	if err != errStopped {
		t.Errorf("err = %v, want %v", err, errStopped)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Run took %s, want claude killed on cancel", took)
	}
}

func TestClaudeNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := NewClaude(t.TempDir(), 5*time.Second).Run(context.Background(), "x", nil)
	if err != errUnreachable {
		t.Errorf("err = %v, want %v", err, errUnreachable)
	}
}
