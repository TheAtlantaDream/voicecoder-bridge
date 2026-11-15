package clean

import (
	"regexp"
	"strings"
)

var (
	// Wake phrases anywhere in the utterance (case-insensitive):
	// "hey VoiceCoder", "VoiceCoder", "hey Coder", "Coder".
	// \W / ^ keeps "encoder"/"decoder" from counting as a wake word.
	// Everything before the first match is discarded; the request is what follows.
	wakeRe = regexp.MustCompile(`(?i)(?:^|\W)(?:hey\s+)?(?:voice\s*)?coder\b[.,!\s]*`)

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

// interruptRe is the stricter phrase that cuts in on the agent or on a
// spoken summary: it needs "hey", so a passing mention of "coder" in the
// room does not cancel work in progress.
var interruptRe = regexp.MustCompile(`(?i)(?:^|\W)hey[\s,.!]*(?:voice\s*)?coder\b`)

// HasInterruptPhrase reports whether text contains "hey Coder" or
// "hey VoiceCoder".
func HasInterruptPhrase(text string) bool {
	return interruptRe.MatchString(text)
}

func HasWakeWord(text string) bool {
	return wakeRe.FindStringIndex(text) != nil
}

// StripWakeWord returns the text after the first wake phrase, dropping any
// preamble before it. If there is no wake phrase, the original text is returned.
func StripWakeWord(text string) string {
	loc := wakeRe.FindStringIndex(text)
	if loc == nil {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(text[loc[1]:])
}

// endWordRe builds a word-boundary matcher for an optional spoken end phrase
// (for example "over" or "send"). Empty word disables matching.
func endWordRe(word string) *regexp.Regexp {
	word = strings.TrimSpace(word)
	if word == "" {
		return nil
	}
	return regexp.MustCompile(`(?i)(?:^|\W)` + regexp.QuoteMeta(word) + `\b[.,!\s]*`)
}

// HasEndWord reports whether text contains the configured end word as a
// whole word. An empty end word never matches.
func HasEndWord(text, word string) bool {
	re := endWordRe(word)
	if re == nil {
		return false
	}
	return re.FindStringIndex(text) != nil
}

// StripEndWord returns the text before the first end word, dropping the end
// word and anything after it. If there is no end word (or word is empty),
// the original text is returned.
func StripEndWord(text, word string) string {
	re := endWordRe(word)
	if re == nil {
		return strings.TrimSpace(text)
	}
	loc := re.FindStringIndex(text)
	if loc == nil {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(text[:loc[0]])
}

func Clean(text string) string {
	return CleanEnd(text, "")
}

// CleanEnd is Clean plus an optional end word: after the wake phrase is
// stripped, the end word and everything after it are dropped.
func CleanEnd(text, endWord string) string {
	text = StripWakeWord(text)
	text = StripEndWord(text, endWord)

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
