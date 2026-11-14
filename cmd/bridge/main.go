package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/theatlantadream/voicecoder-bridge/internal/bridge"
	"github.com/theatlantadream/voicecoder-bridge/internal/whisper"
)

func main() {
	addr := flag.String("addr", ":8000", "listen address for the WebSocket server")
	whisperURL := flag.String("whisper-url", "http://localhost:8080", "base URL of the local whisper.cpp server")
	silenceRMS := flag.Float64("silence-rms", 500, "16-bit RMS level below which counts as silence")
	silenceSecs := flag.Float64("silence-secs", 1.5, "seconds of silence after speech that end an utterance")
	flag.Parse()

	log.Printf("voicecoder bridge: addr %s whisper=%s", *addr, *whisperURL)

	transcriber := whisper.NewHTTPClient(*whisperURL, 60*time.Second)

	srv := bridge.NewServer(transcriber, bridge.WithSilence(*silenceRMS, time.Duration(*silenceSecs*float64(time.Second))))

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
