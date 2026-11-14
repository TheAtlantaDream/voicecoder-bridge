package clean

import (
	"regexp"
	"strings"
)

var (
	// Wake phrases, case-insensitive, only at the start of the utterance.
	wakeRe = regexp.MustCompile(`(?i)^\s*(hey\s+)?voice\s*coder[.,\s]*`)

	// Spoken "dot" between two word characters: "auth dot py" -> "auth.py".
	// The spaces around "dot" are consumed so no stray space remains.
	dotRe = regexp.MustCompile(`(?i)(\w)\s+dot\s+(\w)`)

	// One or more whitespace runs collapse to a single space.
	spaceRe = regexp.MustCompile(`\s+`)

	// A space before punctuation ("hello ." -> "hello.").
	punctRe = regexp.MustCompile(`\s+([.,;:!?])`)
)

var fillers = map[string]bool{
	"um": true, "uh": true, "erm": true, "hmm": true,
}

func HasWakeWord(text string) bool {
	return wakeRe.MatchString(text)
}

func StripWakeWord(text string) string {
	return strings.TrimSpace(wakeRe.ReplaceAllString(text, ""))
}

func Clean(text string) string {
	text = StripWakeWord(text)

	words := spaceRe.Split(strings.TrimSpace(text), -1)
	kept := words[:0]
	for _, w := range words {
		if fillers[strings.ToLower(w)] {
			continue
		}
		kept = append(kept, w)
	}
	text = strings.Join(kept, " ")
	repeated := spaceRe.Split(text, -1)
	deduped := repeated[:0]
	for _, w := range repeated {
		if len(deduped) > 0 && strings.EqualFold(w, deduped[len(deduped)-1]) {
			continue
		}
		deduped = append(deduped, w)
	}
	text = strings.Join(deduped, " ")

	text = dotRe.ReplaceAllString(text, "$1.$2")

	text = punctRe.ReplaceAllString(text, "$1")
	text = spaceRe.ReplaceAllString(strings.TrimSpace(text), " ")
	return text
}
