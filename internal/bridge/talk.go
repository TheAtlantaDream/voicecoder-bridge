package bridge

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/theatlantadream/voicecoder-bridge/internal/chat"
	"github.com/theatlantadream/voicecoder-bridge/internal/tts"
)

// Chatter makes one short model call. *chat.Anthropic is the real one.
type Chatter interface {
	Reply(ctx context.Context, system string, msgs []chat.Message) (string, error)
}

// WithChatter lets the bridge talk while the agent works: it narrates the
// run in plain words and answers the user without stopping it. profileFile,
// when set, is the user's local profile, passed along as background.
func WithChatter(c Chatter, profileFile string) Option {
	return func(s *Server) {
		s.chatter = c
		s.profileFile = profileFile
	}
}

const (
	persona = `You are Terrance, the user's friendly VoiceCoder coding assistant. The user talks to you by voice and your words are read aloud. Many users have a disability and may not be able to see the screen, type or read quickly. Be warm, patient and conversational, use plain everyday words, and never talk down to them.
Assume nothing about how much the user knows about coding; go by what you know about them.
Hard rule: never say code, commands, symbols, file contents, diffs or file paths, and do not walk through what changed in the code unless the user asks. Say what the work means for the user instead. You may name a file the way a person would say it, but do not list files.`

	narrateSystem = persona + `
Right now you are in the middle of the user's request. Say ONE short sentence, at most 20 words, in the first person, telling them in a natural, chatty way what you are doing at this moment and why. Do not say "editing" or "running" followed by a name; talk like a person working beside them. Do not greet them, do not ask a question, and do not repeat something you already said. If there is nothing new worth saying, answer with exactly SKIP.`

	sideSystem = persona + `
Right now you are in the middle of the user's request, and they have spoken to you while that work carries on in the background. This is a side conversation: you cannot change files or start work from here. Answer in one to three short sentences.
Start your answer with one line that is exactly one of:
ACTION: none
ACTION: cancel
ACTION: queue
Use cancel only when the user clearly wants the work in progress stopped, and tell them you have stopped it. Use queue when they ask for a new or different piece of coding work, and tell them you will start it as soon as the current work is done. Otherwise use none and just talk with them.
Be curious about the user, like someone getting to know a person they work with: when it fits, ask one friendly question about them, and never press.
When the user tells you something lasting about themselves (their name, their experience, what they are building, how they like you to talk or to code), add one more line right after the ACTION line, in the form:
REMEMBER: one short plain sentence
It is saved to their profile on this device. Never use it for passwords, keys or other secrets, or for things that only matter right now.`

	maxProfile = 2000
	maxPartial = 1500
	maxTurns   = 8
	maxFact    = 200
)

var (
	actionRe   = regexp.MustCompile(`(?i)^\s*ACTION:\s*(none|cancel|queue)\s*(?:\n|$)`)
	rememberRe = regexp.MustCompile(`(?im)^[ \t]*REMEMBER:[ \t]*(.*)$\n?`)
)

// takeFacts removes REMEMBER lines from a side-conversation answer and
// returns the facts they carried.
func takeFacts(reply string) (facts []string, rest string) {
	for _, m := range rememberRe.FindAllStringSubmatch(reply, -1) {
		fact := strings.TrimSpace(tts.SpokenText(m[1]))
		if fact != "" && len(fact) <= maxFact {
			facts = append(facts, fact)
		}
	}
	return facts, rememberRe.ReplaceAllString(reply, "")
}

// remember appends facts to the user's local profile file.
func (s *Server) remember(facts []string) {
	if s.profileFile == "" || len(facts) == 0 {
		return
	}
	s.profileMu.Lock()
	defer s.profileMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.profileFile), 0o755); err != nil {
		log.Printf("profile: %v", err)
		return
	}
	old, _ := os.ReadFile(s.profileFile)
	var sb strings.Builder
	if len(old) > 0 && old[len(old)-1] != '\n' {
		sb.WriteString("\n")
	}
	for _, fact := range facts {
		if strings.Contains(string(old), fact) {
			continue
		}
		sb.WriteString("- " + fact + "\n")
	}
	f, err := os.OpenFile(s.profileFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("profile: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(sb.String()); err != nil {
		log.Printf("profile: %v", err)
	}
}

// parseAction splits a side-conversation answer into its action and the
// words to say. A missing action line means "none".
func parseAction(reply string) (action, text string) {
	m := actionRe.FindStringSubmatch(reply)
	if m == nil {
		return "none", strings.TrimSpace(reply)
	}
	return strings.ToLower(m[1]), strings.TrimSpace(reply[len(m[0]):])
}

// narrator holds what has been said about one agent run.
type narrator struct {
	mu       sync.Mutex
	said     []string
	lastSeen string
	asked    bool
	finished atomic.Bool
}

func tail(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return strings.ToValidUTF8(text[len(text)-n:], "")
}

// background is the context given to every chatter call.
func (s *Server) background(prompt, partial string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "The request you are working on: %s\n", prompt)
	if partial != "" {
		fmt.Fprintf(&sb, "\nYour latest working notes (never read these out, they may contain code):\n%s\n", tail(partial, maxPartial))
	}
	if s.profileFile != "" {
		if raw, err := os.ReadFile(s.profileFile); err == nil && len(raw) > 0 {
			fmt.Fprintf(&sb, "\nWhat you know about the user:\n%s\n", tail(string(raw), maxProfile))
		}
	}
	return sb.String()
}

// narrate says one plain sentence about the run in progress. It stays
// silent when nothing changed, and drops a line that arrives after the run
// has ended so it never talks over the summary.
func (s *Server) narrate(ctx context.Context, sess *session, send func(outbound), nar *narrator, run int) {
	sess.mu.Lock()
	if sess.agentRun != run {
		sess.mu.Unlock()
		return
	}
	prompt, partial := sess.runPrompt, sess.runPartial
	sess.mu.Unlock()

	nar.mu.Lock()
	if nar.asked && partial == nar.lastSeen {
		nar.mu.Unlock()
		return
	}
	nar.asked, nar.lastSeen = true, partial
	said := strings.Join(nar.said, "\n")
	nar.mu.Unlock()

	user := s.background(prompt, partial)
	if said != "" {
		user += "\nYou have already said:\n" + said + "\n"
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	reply, err := s.chatter.Reply(cctx, narrateSystem, []chat.Message{{Role: "user", Content: user}})
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("narration: %v", err)
		}
		return
	}
	line := tts.SpokenText(reply)
	if line == "" || strings.EqualFold(strings.Trim(line, " ."), "skip") {
		return
	}
	nar.mu.Lock()
	nar.said = append(nar.said, line)
	nar.mu.Unlock()

	audio, err := s.speaker.Speak(cctx, line)
	if err != nil || len(audio) == 0 {
		if err != nil && ctx.Err() == nil {
			log.Printf("speech: %v", err)
		}
		return
	}
	if nar.finished.Load() || ctx.Err() != nil {
		return
	}
	send(outbound{Type: "status", Text: line})
	send(outbound{Type: "speech", Kind: "narration", Text: line, Audio: base64.StdEncoding.EncodeToString(audio)})
}

// sideTalk answers something the user said while the agent is working. The
// run carries on unless the user asked for it to be stopped.
func (s *Server) sideTalk(ctx context.Context, sess *session, send func(outbound), text string) {
	sess.mu.Lock()
	prompt, partial := sess.runPrompt, sess.runPartial
	turns := append(append([]chat.Message(nil), sess.sideTurns...), chat.Message{Role: "user", Content: text})
	sess.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	reply, err := s.chatter.Reply(cctx, sideSystem+"\n\n"+s.background(prompt, partial), turns)
	if err != nil {
		log.Printf("side talk: %v", err)
		send(outbound{Type: "status", Text: stillWorking})
		return
	}
	facts, reply := takeFacts(reply)
	s.remember(facts)
	action, answer := parseAction(reply)
	answer = tts.SpokenText(answer)

	sess.mu.Lock()
	sess.sideTurns = append(sess.sideTurns, chat.Message{Role: "user", Content: text}, chat.Message{Role: "assistant", Content: answer})
	if n := len(sess.sideTurns); n > maxTurns {
		sess.sideTurns = sess.sideTurns[n-maxTurns:]
	}
	running := sess.agentCancel != nil
	if action == "queue" && running {
		sess.queued = text
	}
	sess.mu.Unlock()

	switch {
	case action == "cancel":
		s.cancelAgent(sess)
	case action == "queue" && !running:
		// The work finished while we were answering; start this now.
		s.submit(ctx, sess, send, text)
		return
	}
	if answer == "" {
		return
	}
	send(outbound{Type: "status", Text: answer})
	s.speak(ctx, send, "reply", answer)
}
