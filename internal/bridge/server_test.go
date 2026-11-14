package bridge

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/theatlantadream/voicecoder-bridge/internal/whisper"
)

// fakeTranscriber returns one canned line per chunk, so the test can check
// joining without a running Whisper server.
type fakeTranscriber struct {
	lines []string
	i     int
}

func (f *fakeTranscriber) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if f.i >= len(f.lines) {
		return "", nil
	}
	line := f.lines[f.i]
	f.i++
	return line, nil
}

var _ whisper.Transcriber = (*fakeTranscriber)(nil)

// TestFinishJoinsPartialsInOrder checks the core contract Stephen asked
// for: partials are joined in sequence order at "end", and cleaning
// happens once on the joined text, not per partial.
func TestFinishJoinsPartialsInOrder(t *testing.T) {
	srv := NewServer(&fakeTranscriber{})

	// Simulate three chunks that finished out of order: seq 1 arrived
	// before seq 0. The join must still read 0, 1, 2.
	sess := &session{partials: map[int]string{
		1: "the login bug",
		0: "hey VoiceCoder fix",
		2: "in auth dot py",
	}}

	var got []outbound
	send := func(m outbound) { got = append(got, m) }

	srv.finishUtterance(context.Background(), sess, send)

	var transcript, cleaned string
	for _, m := range got {
		switch m.Type {
		case "transcript":
			transcript = m.Text
		case "cleaned":
			cleaned = m.Text
		}
	}
	if transcript != "hey VoiceCoder fix the login bug in auth dot py" {
		t.Errorf("transcript = %q, want the three partials joined in order", transcript)
	}
	// Cleaning runs once, on the joined text: wake word stripped, and
	// "auth dot py" normalised, only here — not on any single partial.
	if cleaned != "fix the login bug in auth.py" {
		t.Errorf("cleaned = %q, want %q", cleaned, "fix the login bug in auth.py")
	}
}

// TestTranscribeChunkDropsBlankAudio checks that a chunk Whisper marks as
// non-speech ("[BLANK_AUDIO]") is never stored as a partial or sent to the
// frontend, so it cannot leak into the joined transcript.
func TestTranscribeChunkDropsBlankAudio(t *testing.T) {
	srv := NewServer(&fakeTranscriber{lines: []string{"[BLANK_AUDIO]"}})
	sess := &session{partials: make(map[int]string)}

	var got []outbound
	send := func(m outbound) { got = append(got, m) }

	sess.pending.Add(1) // transcribeChunk always defers Done().
	srv.transcribeChunk(context.Background(), sess, send, 0, []byte{1, 2, 3})
	sess.pending.Wait()

	if len(sess.partials) != 0 {
		t.Errorf("partials = %v, want none stored for a blank chunk", sess.partials)
	}
	if len(got) != 0 {
		t.Errorf("sent %d messages, want none for a blank chunk", len(got))
	}
}

// TestRMSOf checks the loudness measure behind hands-free ending:
// silence is 0, a steady tone reads back its own amplitude.
func TestRMSOf(t *testing.T) {
	if got := rmsOf([]byte{0, 0, 0, 0}); got != 0 {
		t.Errorf("rmsOf(silence) = %v, want 0", got)
	}
	pcm := make([]byte, 8) // four samples of +1000
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint16(pcm[2*i:], 1000)
	}
	if got := rmsOf(pcm); got != 1000 {
		t.Errorf("rmsOf(1000) = %v, want 1000", got)
	}
}

// TestAutoEndDue checks the hands-free ending rule: it fires only after
// speech has started and the silence has lasted long enough. Silence
// before any speech never ends an utterance.
func TestAutoEndDue(t *testing.T) {
	srv := NewServer(&fakeTranscriber{})
	now := time.Now()

	sess := &session{listening: true, speechStarted: false, lastSpeechAt: now.Add(-10 * time.Second)}
	if srv.autoEndDue(sess, now) {
		t.Errorf("autoEndDue = true before any speech, want false")
	}

	sess.speechStarted = true
	sess.lastSpeechAt = now.Add(-500 * time.Millisecond)
	if srv.autoEndDue(sess, now) {
		t.Errorf("autoEndDue = true after 0.5s of silence, want false (timeout is 1.5s)")
	}

	sess.lastSpeechAt = now.Add(-2 * time.Second)
	if !srv.autoEndDue(sess, now) {
		t.Errorf("autoEndDue = false after 2s of silence, want true")
	}

	sess.listening = false
	if srv.autoEndDue(sess, now) {
		t.Errorf("autoEndDue = true after the utterance ended, want false")
	}
}
