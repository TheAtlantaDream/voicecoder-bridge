// Package chat makes short, quick Claude calls for the bridge itself:
// narrating a running agent and answering the user while it works. The
// coding agent is a separate, long-lived Claude session.
package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultURL   = "https://api.anthropic.com"
	defaultModel = "claude-haiku-4-5"
	apiVersion   = "2023-06-01"
	maxTokens    = 300
)

// Message is one turn of a conversation. Role is "user" or "assistant".
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Anthropic calls the Anthropic Messages API.
type Anthropic struct {
	key, model, url string
	client          *http.Client
}

// NewAnthropic returns a client. Empty model and url use the defaults.
func NewAnthropic(key, model, url string) *Anthropic {
	if strings.TrimSpace(model) == "" {
		model = defaultModel
	}
	if strings.TrimSpace(url) == "" {
		url = defaultURL
	}
	return &Anthropic{
		key:    key,
		model:  strings.TrimSpace(model),
		url:    strings.TrimRight(strings.TrimSpace(url), "/"),
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

// Reply returns the model's text answer to msgs under the system prompt.
func (a *Anthropic) Reply(ctx context.Context, system string, msgs []Message) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      a.model,
		"max_tokens": maxTokens,
		"system":     system,
		"messages":   msgs,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", apiVersion)

	resp, err := a.client.Do(req)
	if err != nil {
		return "", errors.New("chat: request failed") // never echo the request
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return "", fmt.Errorf("chat: status %d: %s", resp.StatusCode, e.Error.Message)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", errors.New("chat: unreadable reply")
	}
	var sb strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", errors.New("chat: empty reply")
	}
	return text, nil
}
