package clean

import "testing"

func TestClean(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hey VoiceCoder fix the login bug in auth dot py", "fix the login bug in auth.py"},
		{"hey Coder fix the login bug", "fix the login bug"},
		{"Coder add a test", "add a test"},
		{"VoiceCoder rename the file", "rename the file"},
		{"um hey VoiceCoder fix the login bug", "fix the login bug"},
		{"okay VoiceCoder, add a test for auth dot py", "add a test for auth.py"},
		{"please hey Coder rename the file", "rename the file"},
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
	for _, text := range []string{
		"hey VoiceCoder fix this",
		"VoiceCoder fix this",
		"hey Coder fix this",
		"Coder fix this",
		"hey coder, add a test",
		"um hey VoiceCoder fix this",
		"okay, Coder add a test",
	} {
		if !HasWakeWord(text) {
			t.Errorf("expected wake word in %q", text)
		}
	}
	for _, text := range []string{
		"fix this bug",
		"coding a function",
		"please fix auth",
		"please fix the encoder",
		"look at the decoder output",
	} {
		if HasWakeWord(text) {
			t.Errorf("expected no wake word in %q", text)
		}
	}
}

func TestStripWakeWordKeepsTextAfter(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hey VoiceCoder fix this", "fix this"},
		{"um hey Coder fix this", "fix this"},
		{"okay VoiceCoder, rename auth.py", "rename auth.py"},
		{"no wake word here", "no wake word here"},
	}
	for _, c := range cases {
		if got := StripWakeWord(c.in); got != c.want {
			t.Errorf("StripWakeWord(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
