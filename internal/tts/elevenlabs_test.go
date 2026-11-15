package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSpeakSendsRequestAndReturnsAudio(t *testing.T) {
	var gotPath, gotQuery, gotKey string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotKey = r.URL.Path, r.URL.RawQuery, r.Header.Get("xi-api-key")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("mp3-bytes"))
	}))
	defer srv.Close()

	audio, err := NewElevenLabs("secret", "", "", srv.URL).Speak(context.Background(), "  I added a test.  ")
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if string(audio) != "mp3-bytes" {
		t.Errorf("audio = %q, want the response body", audio)
	}
	if gotPath != "/v1/text-to-speech/"+DefaultVoice || gotQuery != "output_format=mp3_44100_128" {
		t.Errorf("request = %s?%s, want the default voice and mp3 output", gotPath, gotQuery)
	}
	if gotKey != "secret" {
		t.Errorf("xi-api-key = %q, want the configured key", gotKey)
	}
	if gotBody["text"] != "I added a test." || gotBody["model_id"] != DefaultModel {
		t.Errorf("body = %v, want trimmed text and the default model", gotBody)
	}
}

func TestSpeakUsesConfiguredVoiceAndModel(t *testing.T) {
	var gotPath, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body["model_id"]
	}))
	defer srv.Close()

	if _, err := NewElevenLabs("k", "voice1", "model1", srv.URL+"/").Speak(context.Background(), "hi"); err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if gotPath != "/v1/text-to-speech/voice1" || gotModel != "model1" {
		t.Errorf("got path %q model %q, want voice1 and model1", gotPath, gotModel)
	}
}

func TestSpeakErrorHidesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"quota exceeded"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := NewElevenLabs("very-secret-key", "", "", srv.URL).Speak(context.Background(), "hi")
	if err == nil {
		t.Fatal("Speak returned no error for a 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("error = %v, want the status and the service's reason", err)
	}
	if strings.Contains(err.Error(), "very-secret-key") {
		t.Errorf("error leaks the API key: %v", err)
	}
}

func TestSpeakEmptyTextMakesNoRequest(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()

	audio, err := NewElevenLabs("k", "", "", srv.URL).Speak(context.Background(), "   ")
	if err != nil || audio != nil || called {
		t.Errorf("audio=%v err=%v called=%v, want nothing for blank text", audio, err, called)
	}
}

func TestSpeakCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewElevenLabs("k", "", "", srv.URL).Speak(ctx, "hi"); err == nil {
		t.Error("Speak returned no error for a cancelled context")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate short = %q", got)
	}
	if got := truncate("one two three four", 12); got != "one two" {
		t.Errorf("truncate = %q, want a cut on a word boundary", got)
	}
	if got := truncate(strings.Repeat("é", 20), 5); got != strings.Repeat("é", 5) {
		t.Errorf("truncate = %q, want 5 whole characters", got)
	}
}
