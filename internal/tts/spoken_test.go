package tts

import "testing"

func TestSpokenText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"summary only", "I edited main.go:\n```go\nfunc main() {}\n```\nSummary: I added a main function. It prints hello.", "I added a main function. It prints hello."},
		{"summary on next lines", "Details here.\n\n**Summary:**\nAdded the file.\nTests passed.", "Added the file. Tests passed."},
		{"heading form", "stuff\n## Summary\n- Created `hello.txt`\n- Nothing else changed", "Created hello.txt Nothing else changed"},
		{"last marker wins", "Summary: first\nmore work\nSummary: second", "second"},
		{"no marker drops code", "Here it is:\n```py\nprint('x')\n```\nI wrote **one** line.", "Here it is: I wrote one line."},
		{"unclosed fence", "Done.\n```js\nlet a = 1", "Done."},
		{"code kept when in summary", "Summary: The function is:\n```go\nfunc a() {}\n```", "The function is: func a() {}"},
		{"links", "Summary: See [the docs](https://x.y/z) for more.", "See the docs for more."},
		{"word summary mid sentence is not a marker", "In summary this is fine.", "In summary this is fine."},
		{"summary of is not a marker", "Summary of changes follows.\nSummary: Done.", "Done."},
		{"sentence starting with summary", "Summary of what I did is below.", "Summary of what I did is below."},
		{"empty", "  \n ", ""},
	}
	for _, c := range cases {
		if got := SpokenText(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
