// Command opencode-cc is a local reverse proxy that lets Claude Code talk to
// OpenCode Zen (and other OpenAI-compatible endpoints) by translating the
// Anthropic Messages API to OpenAI Chat Completions on the fly, with a
// built-in web control panel.
//
// Quick start:
//
//	set ZEN_API_KEY=sk-...
//	opencode-cc.exe
//	# then point Claude Code at it:
//	set ANTHROPIC_BASE_URL=http://localhost:8787
//	set ANTHROPIC_AUTH_TOKEN=anything
//	claude
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Kiowx/opencode-cc/internal/assets"
	"github.com/Kiowx/opencode-cc/internal/config"
	"github.com/Kiowx/opencode-cc/internal/server"
	"github.com/Kiowx/opencode-cc/internal/store"
)

func main() {
	dataDir := flag.String("data", "data", "directory for config + SQLite database")
	flag.Parse()

	abs, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("resolve data dir: %v", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	cfg, err := config.Load(abs)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	st, err := store.Open(filepath.Join(abs, "opencode-cc.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer st.Close()

	srv := server.New(cfg, st)

	// Resolve panel assets (embedded build or on-disk web/dist).
	panelFS, hasAssets := assets.FileSystem()
	var panelHandler http.Handler
	if hasAssets {
		panelHandler = assets.SPAMux(panelFS)
	}

	handler := srv.Handler(panelFS, panelHandler)

	addr := cfg.Snapshot().ListenAddr
	log.Printf("opencode-cc listening on http://%s  (panel: %v, data: %s)", addr, hasAssets, abs)
	if cfg.Snapshot().ZenAPIKey == "" {
		log.Printf("WARNING: no ZEN_API_KEY set — set it via env var or the web panel at http://%s", addr)
	} else {
		log.Printf("upstream: %s   default model: %s", cfg.Snapshot().UpstreamBase, cfg.Snapshot().DefaultModel)
	}

	// Graceful shutdown on Ctrl+C / service stop.
	go func() {
		if err := srv.Start(handler); err != nil {
			log.Fatalf("server: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down...")
	_ = context.Background()
}
