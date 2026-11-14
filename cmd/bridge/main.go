package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/theatlantadream/voicecoder-bridge/internal/agent"
	"github.com/theatlantadream/voicecoder-bridge/internal/bridge"
	"github.com/theatlantadream/voicecoder-bridge/internal/whisper"
)

func main() {
	addr := flag.String("addr", ":8000", "listen address for the WebSocket server")
	whisperURL := flag.String("whisper-url", "http://localhost:8080", "base URL of the local whisper.cpp server")
	silenceRMS := flag.Float64("silence-rms", 500, "16-bit RMS level below which counts as silence")
	silenceSecs := flag.Float64("silence-secs", 1.5, "seconds of silence after speech that end an utterance")
	agentName := flag.String("agent", "none", "coding agent to send cleaned requests to: none, fake, or agentapi")
	agentAPIURL := flag.String("agentapi-url", "http://localhost:3284", "base URL of the AgentAPI server")
	repo := flag.String("repo", "~/project", "project folder the agent works in")
	agentTimeout := flag.Duration("agent-timeout", 180*time.Second, "longest one agent request may run")
	allowedOrigins := flag.String("allowed-origins", "http://localhost,http://127.0.0.1,https://coder.c1.workshop.cdr.dev", "comma-separated web origins that may connect when an agent is set")
	flag.Parse()

	log.Printf("voicecoder bridge: addr %s whisper=%s", *addr, *whisperURL)

	transcriber := whisper.NewHTTPClient(*whisperURL, 60*time.Second)

	opts := []bridge.Option{bridge.WithSilence(*silenceRMS, time.Duration(*silenceSecs*float64(time.Second)))}
	switch *agentName {
	case "none":
		log.Printf("agent: none (transcribe and clean only)")
	case "fake":
		opts = append(opts, bridge.WithAgent(agent.Fake{Delay: time.Second}))
		log.Printf("agent: fake (no code is changed)")
	case "agentapi":
		dir := expandHome(*repo)
		opts = append(opts, bridge.WithAgent(agent.NewAgentAPI(*agentAPIURL, dir, *agentTimeout)))
		log.Printf("agent: agentapi url=%s repo=%s timeout=%s", *agentAPIURL, dir, *agentTimeout)
	default:
		log.Fatalf("unknown -agent %q, want none, fake, or agentapi", *agentName)
	}
	if *agentName != "none" {
		origins := strings.Split(*allowedOrigins, ",")
		opts = append(opts, bridge.WithAllowedOrigins(origins))
		log.Printf("allowed origins: %s", strings.Join(origins, ", "))
	}

	srv := bridge.NewServer(transcriber, opts...)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWS)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	httpSrv := &http.Server{Addr: *addr, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("listening on %s (ws endpoint: /ws, health: /health)", *addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}
