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
	"github.com/mehuli-id/sapo/internal/audit"
	"github.com/mehuli-id/sapo/internal/auth"
	"github.com/mehuli-id/sapo/internal/config"
)

var version = "0.0.0-dev"

func main() {
	cfgPath := flag.String("config", config.DefaultPath, "lokasi sapo.yaml")
	listen := flag.String("listen", "", "timpa api.listen (untuk pengembangan)")
	socket := flag.String("socket", "", "timpa agent.socket (untuk pengembangan)")
	webDir := flag.String("web", "", "timpa api.web_dir (untuk pengembangan)")
	brandingFile := flag.String("branding", "", "timpa api.branding_file (untuk pengembangan)")
	wizardsDir := flag.String("wizards", "", "timpa api.wizards_dir (untuk pengembangan)")
	dataDir := flag.String("data", "", "timpa api.data_dir (untuk pengembangan)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("konfigurasi", "err", err)
		os.Exit(1)
	}
	for flagVal, target := range map[*string]*string{
		listen: &cfg.API.Listen, socket: &cfg.Agent.Socket, webDir: &cfg.API.WebDir,
		brandingFile: &cfg.API.BrandingFile, wizardsDir: &cfg.API.WizardsDir, dataDir: &cfg.API.DataDir,
	} {
		if *flagVal != "" {
			*target = *flagVal
		}
	}
	if os.Geteuid() == 0 {
		log.Error("sapo-api tidak boleh berjalan sebagai root; jalankan sebagai user sapo")
		os.Exit(1)
	}

	store, err := auth.OpenStore(cfg.API.DataDir)
	if err != nil {
		log.Error("penyimpanan user", "err", err)
		os.Exit(1)
	}
	auditLog, err := audit.Open(cfg.API.DataDir)
	if err != nil {
		log.Error("log audit", "err", err)
		os.Exit(1)
	}
	defer auditLog.Close()

	handler, err := api.New(api.Options{
		Config: cfg,
		Agent:  &agent.Client{Socket: cfg.Agent.Socket, Timeout: 30 * time.Second},
		Store:  store,
		Audit:  auditLog,
		Log:    log,
	})
	if err != nil {
		log.Error("menyiapkan API", "err", err)
		os.Exit(1)
	}
	httpSrv := &http.Server{
		Addr:              cfg.API.Listen,
		Handler:           handler,
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

	if store.SetupRequired() {
		log.Warn("setup awal belum dilakukan: buka panel dari jaringan lokal untuk membuat akun administrator")
	}
	log.Info("sapo-api siap", "versi", version, "alamat", cfg.API.Listen, "jaringan_diizinkan", cfg.API.AllowedNetworks)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server berhenti", "err", err)
		os.Exit(1)
	}
	log.Info("sapo-api berhenti")
}
