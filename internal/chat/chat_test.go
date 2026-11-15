package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReplySendsKeyAndReadsText(t *testing.T) {
	var got map[string]any
	var key, version string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, version = r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":" Hello "},{"type":"text","text":"there."}]}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k-123", "", srv.URL+"/")
	text, err := a.Reply(context.Background(), "be kind", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hello there." {
		t.Errorf("text = %q", text)
	}
	if key != "k-123" || version != apiVersion {
		t.Errorf("headers: key=%q version=%q", key, version)
	}
	if got["model"] != defaultModel || got["system"] != "be kind" {
		t.Errorf("body = %v", got)
	}
}

func TestReplyErrorHidesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"credit balance is too low"}}`))
	}))
	defer srv.Close()

	_, err := NewAnthropic("k-secret", "m", srv.URL).Reply(context.Background(), "", []Message{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "credit balance") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "k-secret") {
		t.Error("error leaks the key")
	}
}

func TestReplyEmptyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[]}`))
	}))
	defer srv.Close()
	if _, err := NewAnthropic("k", "m", srv.URL).Reply(context.Background(), "", nil); err == nil {
		t.Fatal("want error")
	}
}
