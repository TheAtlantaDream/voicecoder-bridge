package agent

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	diffDirName  = ".voicecoder"
	diffFileName = "latest.diff"
)

// LatestDiffPath is the project-relative path VS Code opens after a run.
const LatestDiffPath = diffDirName + "/" + diffFileName

// WriteLatestDiff writes the unified diff into repo/.voicecoder/latest.diff
// so the editor can open it. An empty diff removes the file. It also ensures
// .voicecoder/ is listed in .gitignore so the patch is never committed.
func WriteLatestDiff(repo, diff string) error {
	dir := filepath.Join(repo, diffDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := ensureGitignore(repo); err != nil {
		return err
	}
	path := filepath.Join(dir, diffFileName)
	if strings.TrimSpace(diff) == "" {
		_ = os.Remove(path)
		return nil
	}
	return os.WriteFile(path, []byte(diff), 0o644)
}

func ensureGitignore(repo string) error {
	path := filepath.Join(repo, ".gitignore")
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(raw)
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == diffDirName+"/" || strings.TrimSpace(line) == diffDirName {
			return nil
		}
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += diffDirName + "/\n"
	return os.WriteFile(path, []byte(text), 0o644)
}
