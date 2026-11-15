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
	"github.com/theatlantadream/voicecoder-bridge/internal/chat"
	"github.com/theatlantadream/voicecoder-bridge/internal/tts"
	"github.com/theatlantadream/voicecoder-bridge/internal/whisper"
)

func main() {
	addr := flag.String("addr", ":8000", "listen address for the WebSocket server")
	whisperURL := flag.String("whisper-url", "http://localhost:8080", "base URL of the local Whisper STT server")
	silenceRMS := flag.Float64("silence-rms", 500, "16-bit RMS level below which counts as silence")
	silenceSecs := flag.Float64("silence-secs", 1.5, "seconds of silence after speech that end an utterance")
	endWord := flag.String("end-word", "", "optional spoken word that ends an utterance early (e.g. over, send); empty keeps silence-only ending; BRIDGE_END_WORD is used when this flag is empty")
	agentName := flag.String("agent", "none", "coding agent to send cleaned requests to: none, fake, agentapi, or claude")
	agentAPIURL := flag.String("agentapi-url", "http://localhost:3284", "base URL of the AgentAPI server")
	repo := flag.String("repo", "~/project", "project folder the agent works in")
	agentTimeout := flag.Duration("agent-timeout", 180*time.Second, "longest one agent request may run")
	allowedOrigins := flag.String("allowed-origins", "http://localhost,http://127.0.0.1,https://coder.c1.workshop.cdr.dev", "comma-separated web origins that may connect when an agent is set; *.example.com matches subdomains; BRIDGE_ALLOWED_ORIGINS adds more")
	flag.Parse()

	log.Printf("voicecoder bridge: addr %s whisper=%s", *addr, *whisperURL)

	transcriber := whisper.NewHTTPClient(*whisperURL, 60*time.Second)

	opts := []bridge.Option{bridge.WithSilence(*silenceRMS, time.Duration(*silenceSecs*float64(time.Second)))}
	ew := strings.TrimSpace(*endWord)
	if ew == "" {
		ew = strings.TrimSpace(os.Getenv("BRIDGE_END_WORD"))
	}
	if ew != "" {
		opts = append(opts, bridge.WithEndWord(ew))
		log.Printf("end word: %q (utterance ends on this word or silence)", ew)
	}
	switch *agentName {
	case "none":
		log.Printf("agent: none (transcribe and clean only)")
	case "fake":
		dir := expandHome(*repo)
		opts = append(opts, bridge.WithAgent(agent.Fake{Delay: time.Second}), bridge.WithRepo(dir))
		log.Printf("agent: fake (no code is changed) repo=%s", dir)
	case "agentapi":
		dir := expandHome(*repo)
		opts = append(opts, bridge.WithAgent(agent.NewAgentAPI(*agentAPIURL, dir, *agentTimeout)), bridge.WithRepo(dir))
		log.Printf("agent: agentapi url=%s repo=%s timeout=%s", *agentAPIURL, dir, *agentTimeout)
	case "claude":
		dir := expandHome(*repo)
		opts = append(opts, bridge.WithAgent(agent.NewClaude(dir, *agentTimeout)), bridge.WithRepo(dir))
		log.Printf("agent: claude (direct command, no AgentAPI) repo=%s timeout=%s", dir, *agentTimeout)
	default:
		log.Fatalf("unknown -agent %q, want none, fake, agentapi, or claude", *agentName)
	}
	if *agentName != "none" {
		origins := strings.Split(*allowedOrigins, ",")
		if extra := strings.TrimSpace(os.Getenv("BRIDGE_ALLOWED_ORIGINS")); extra != "" {
			origins = append(origins, strings.Split(extra, ",")...)
		}
		opts = append(opts, bridge.WithAllowedOrigins(origins))
		log.Printf("allowed origins: %s", strings.Join(origins, ", "))
	}

	// The key comes from the environment only, so it never shows up in a
	// process list or a compose file.
	if key := strings.TrimSpace(os.Getenv("ELEVENLABS_API_KEY")); key != "" {
		voice, model := os.Getenv("ELEVENLABS_VOICE_ID"), os.Getenv("ELEVENLABS_MODEL")
		opts = append(opts, bridge.WithSpeaker(tts.NewElevenLabs(key, voice, model, os.Getenv("ELEVENLABS_URL"))))
		log.Printf("speech: elevenlabs (agent summaries are read aloud)")
		// BRIDGE_INTRO replaces the welcome text; "off" turns it off.
		intro := strings.TrimSpace(os.Getenv("BRIDGE_INTRO"))
		if intro == "" {
			intro = bridge.DefaultIntro
		}
		if *agentName != "none" && !strings.EqualFold(intro, "off") {
			marker := strings.TrimSpace(os.Getenv("BRIDGE_INTRO_FILE"))
			if marker == "" && *agentName != "fake" {
				// Next to the project, not in it, so it survives restarts
				// without showing up in diffs.
				marker = filepath.Join(filepath.Dir(expandHome(*repo)), ".voicecoder", "intro-played")
			}
			opts = append(opts, bridge.WithIntro(intro, marker))
			log.Printf("intro: spoken once after the first request (marker=%q)", marker)
		}
	} else {
		log.Printf("speech: none (ELEVENLABS_API_KEY is not set)")
	}

	// A second, quick Claude call lets the bridge narrate a running agent
	// and answer the user without stopping it. BRIDGE_TALK=off turns it off.
	if key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); key != "" && *agentName != "none" &&
		!strings.EqualFold(strings.TrimSpace(os.Getenv("BRIDGE_TALK")), "off") {
		model := strings.TrimSpace(os.Getenv("BRIDGE_TALK_MODEL"))
		if model == "" {
			model = strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
		}
		profile := strings.TrimSpace(os.Getenv("BRIDGE_PROFILE_FILE"))
		if profile == "" {
			profile = filepath.Join(filepath.Dir(expandHome(*repo)), ".voicecoder", "profile.md")
		}
		opts = append(opts, bridge.WithChatter(chat.NewAnthropic(key, model, os.Getenv("ANTHROPIC_BASE_URL")), profile))
		log.Printf("talk: on (narrates the agent and answers while it works; profile=%q)", profile)
	} else {
		log.Printf("talk: off")
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
