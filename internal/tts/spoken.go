package tts

import (
	"regexp"
	"strings"
)

var (
	// A line that starts the closing summary: "Summary:", "**Summary:**",
	// "## Summary". Anything after the label on the same line is kept.
	summaryRe = regexp.MustCompile(`(?im)^[ \t]*(?:#{1,6}[ \t]*[*_]{0,2}summary[*_]{0,2}[ \t]*:?[*_]{0,2}[ \t]*$|[*_]{0,2}summary[*_]{0,2}[ \t]*:[*_]{0,2}[ \t]*|[*_]{0,2}summary[*_]{0,2}[ \t]*$)`)
	fenceRe   = regexp.MustCompile("(?s)```.*?(?:```|$)")
	fenceLine = regexp.MustCompile("(?m)^[ \t]*```.*$")
	linkRe    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	leadRe    = regexp.MustCompile(`(?m)^[ \t]*(?:#{1,6}|>+|[-*+•]|\d+[.)])[ \t]+`)
	spaceRe   = regexp.MustCompile(`\s+`)
)

// SpokenText picks what is read aloud from an agent reply: only the part
// after the last "Summary:" line. A reply without one is spoken with its
// code blocks removed, so code is never read out by accident. Markdown
// symbols are dropped either way.
func SpokenText(reply string) string {
	text := reply
	if locs := summaryRe.FindAllStringIndex(reply, -1); len(locs) > 0 {
		// Code inside the summary is there because the user asked to hear it.
		text = fenceLine.ReplaceAllString(reply[locs[len(locs)-1][1]:], "")
	} else {
		text = fenceRe.ReplaceAllString(reply, " ")
	}
	text = linkRe.ReplaceAllString(text, "$1")
	text = leadRe.ReplaceAllString(text, "")
	text = strings.NewReplacer("`", "", "*", "", "_", " ", "#", "").Replace(text)
	return strings.TrimSpace(spaceRe.ReplaceAllString(text, " "))
}
