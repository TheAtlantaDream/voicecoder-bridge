package clean

import "testing"

func TestClean(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hey VoiceCoder fix the login bug in auth dot py", "fix the login bug in auth.py"},
		{"um fix fix the login bug", "fix the login bug"},
		{"uh run the tests please", "run the tests please"},
		{"fix the login bug in auth.py", "fix the login bug in auth.py"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Clean(c.in); got != c.want {
			t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHasWakeWord(t *testing.T) {
	if !HasWakeWord("hey VoiceCoder fix this") {
		t.Error("expected wake word to be detected")
	}
	if HasWakeWord("fix this bug") {
		t.Error("expected no wake word")
	}
}
