// Package tts turns the agent's summary into spoken audio.
package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultBaseURL = "https://api.elevenlabs.io"
	DefaultVoice   = "JBFqnCBsd6RMkjVDRZzb"
	DefaultModel   = "eleven_flash_v2_5"

	// maxChars bounds what one summary can cost. ElevenLabs bills per character.
	maxChars = 1200
	// maxAudioBytes bounds the reply held in memory and sent to the web app.
	maxAudioBytes = 8 << 20
)

// ElevenLabs calls the ElevenLabs text-to-speech API and returns MP3 audio.
type ElevenLabs struct {
	apiKey  string
	voice   string
	model   string
	baseURL string
	client  *http.Client
}

// NewElevenLabs builds a client. Empty voice, model, or baseURL use the defaults.
func NewElevenLabs(apiKey, voice, model, baseURL string) *ElevenLabs {
	if voice == "" {
		voice = DefaultVoice
	}
	if model == "" {
		model = DefaultModel
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &ElevenLabs{
		apiKey:  apiKey,
		voice:   voice,
		model:   model,
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Speak returns text as MP3 audio. Errors never include the API key.
func (e *ElevenLabs) Speak(ctx context.Context, text string) ([]byte, error) {
	text = truncate(strings.TrimSpace(text), maxChars)
	if text == "" {
		return nil, nil
	}
	body, err := json.Marshal(map[string]string{"text": text, "model_id": e.model})
	if err != nil {
		return nil, err
	}
	endpoint := e.baseURL + "/v1/text-to-speech/" + url.PathEscape(e.voice) + "?output_format=mp3_44100_128"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", e.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/mpeg")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("elevenlabs: status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	audio, err := io.ReadAll(io.LimitReader(resp.Body, maxAudioBytes+1))
	if err != nil {
		return nil, fmt.Errorf("elevenlabs: %w", err)
	}
	if len(audio) > maxAudioBytes {
		return nil, fmt.Errorf("elevenlabs: audio larger than %d bytes", maxAudioBytes)
	}
	return audio, nil
}

// truncate cuts s to at most max characters, ending on a word where it can.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := string(r[:max])
	if i := strings.LastIndexAny(cut, " \n\t"); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut)
}
