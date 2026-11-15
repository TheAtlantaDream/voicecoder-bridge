package bridge

import (
	"regexp"
	"strings"
)

// The agent asks for the site it is serving to be shown or hidden by writing
// a line "Preview: open" or "Preview: close" in its reply.
var previewRe = regexp.MustCompile(`(?mi)^[ \t]*Preview:[ \t]*(open|close)[ \t]*\.?[ \t]*\n?`)

// splitPreview removes preview lines from an agent reply and returns the last
// one's action ("open", "close" or "").
func splitPreview(text string) (string, string) {
	found := previewRe.FindAllStringSubmatch(text, -1)
	if len(found) == 0 {
		return text, ""
	}
	action := strings.ToLower(found[len(found)-1][1])
	return strings.TrimSpace(previewRe.ReplaceAllString(text, "")), action
}
