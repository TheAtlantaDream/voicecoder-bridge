package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteLatestDiff(t *testing.T) {
	repo := t.TempDir()
	diff := "diff --git a/a.go b/a.go\n+hello\n"
	if err := WriteLatestDiff(repo, diff); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repo, LatestDiffPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != diff {
		t.Errorf("latest.diff = %q, want %q", got, diff)
	}
	ignore, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ignore), ".voicecoder/") {
		t.Errorf(".gitignore = %q, want .voicecoder/", ignore)
	}
	if err := WriteLatestDiff(repo, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, LatestDiffPath)); !os.IsNotExist(err) {
		t.Errorf("empty diff should remove the file, err=%v", err)
	}
}
