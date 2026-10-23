// sapo-api adalah server HTTP untuk panel dan aplikasi Sapo. Ia berjalan
// tanpa hak root dan meneruskan semua operasi sistem ke sapod.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mehuli-id/sapo/internal/agent"
	"github.com/mehuli-id/sapo/internal/api"
	"github.com/mehuli-id/sapo/internal/config"
)

var version = "0.0.0-dev"

func main() {
	cfgPath := flag.String("config", config.DefaultPath, "lokasi sapo.yaml")
	listen := flag.String("listen", "", "timpa api.listen (untuk pengembangan)")
	socket := flag.String("socket", "", "timpa agent.socket (untuk pengembangan)")
	webDir := flag.String("web", "", "timpa api.web_dir (untuk pengembangan)")
	brandingFile := flag.String("branding", "", "timpa api.branding_file (untuk pengembangan)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("konfigurasi", "err", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.API.Listen = *listen
	}
	if *socket != "" {
		cfg.Agent.Socket = *socket
	}
	if *webDir != "" {
		cfg.API.WebDir = *webDir
	}
	if *brandingFile != "" {
		cfg.API.BrandingFile = *brandingFile
	}
	if os.Geteuid() == 0 {
		log.Error("sapo-api tidak boleh berjalan sebagai root; jalankan sebagai user sapo")
		os.Exit(1)
	}
	if err := api.EnsureLoopback(cfg.API.Listen); err != nil {
		log.Error("alamat listen", "err", err)
		os.Exit(1)
	}

	client := &agent.Client{Socket: cfg.Agent.Socket, Timeout: 30 * time.Second}
	httpSrv := &http.Server{
		Addr:              cfg.API.Listen,
		Handler:           api.New(cfg, client, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("sapo-api siap", "versi", version, "alamat", "http://"+cfg.API.Listen, "agent", cfg.Agent.Socket)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server berhenti", "err", err)
		os.Exit(1)
	}
	log.Info("sapo-api berhenti")
}
