// sapo-api adalah server HTTP untuk panel dan aplikasi Sapo. Ia berjalan
// tanpa hak root dan meneruskan semua operasi sistem ke sapod.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net"
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
	cfgPath := flag.String("config", config.DefaultPath, "lokasi sapo.yaml milik admin")
	defaultsPath := flag.String("defaults", "", "lokasi nilai bawaan paket (misalnya "+config.DefaultsPath+")")
	listen := flag.String("listen", "", "timpa api.listen (untuk pengembangan)")
	socket := flag.String("socket", "", "timpa agent.socket (untuk pengembangan)")
	webDir := flag.String("web", "", "timpa api.web_dir (untuk pengembangan)")
	brandingFile := flag.String("branding", "", "timpa api.branding_file (untuk pengembangan)")
	wizardsDir := flag.String("wizards", "", "timpa api.wizards_dir (untuk pengembangan)")
	dataDir := flag.String("data", "", "timpa api.data_dir (untuk pengembangan)")
	noTLS := flag.Bool("no-tls", false, "matikan HTTPS; semua lewat api.listen (hanya untuk pengembangan)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.LoadLayered(*defaultsPath, *cfgPath)
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
	auditLog, err := audit.Open(cfg.API.DataDir, int64(cfg.API.AuditMaxMB)<<20)
	if err != nil {
		log.Error("log audit", "err", err)
		os.Exit(1)
	}
	defer auditLog.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	agentClient := &agent.Client{Socket: cfg.Agent.Socket, Timeout: 30 * time.Second}
	// Alamat panel di layar konsol NAS diperbarui lewat sapod (berjalan di
	// latar belakang; sapod mungkin belum siap).
	console := func(scheme, port, fingerprint string) {
		if *noTLS {
			return // mode pengembangan
		}
		go announceConsole(ctx, agentClient, log, scheme, port, fingerprint)
	}

	var certs *certManager
	var httpsInfo *api.HTTPSInfo
	if cfg.API.TLS.Enabled && !*noTLS {
		certs, err = newCertManager(cfg, log, console)
		if err != nil {
			log.Error("sertifikat HTTPS", "err", err)
			os.Exit(1)
		}
		go certs.renewLoop(ctx)
		httpsInfo = &api.HTTPSInfo{Port: certs.port, Fingerprint: certs.fingerprint}
	}

	handler, err := api.New(api.Options{
		Config: cfg,
		Agent:  agentClient,
		Store:  store,
		Audit:  auditLog,
		Log:    log,
		HTTPS:  httpsInfo,
	})
	if err != nil {
		log.Error("menyiapkan API", "err", err)
		os.Exit(1)
	}
	// Akun sistem dan shared folder disamakan dengan data panel.
	go handler.SyncLoop(ctx)
	newServer := func(addr string) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
		}
	}
	servers := []*http.Server{newServer(cfg.API.Listen)}
	if certs != nil {
		srv := newServer(cfg.API.TLS.Listen)
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.get}
		servers = append(servers, srv)
	}
	if certs != nil {
		console("https", certs.port, certs.fingerprint())
	} else if _, port, err := net.SplitHostPort(cfg.API.Listen); err == nil {
		console("http", port, "")
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(shutdownCtx)
		}
	}()

	if store.SetupRequired() {
		log.Warn("setup awal belum dilakukan: buka panel dari jaringan lokal untuk membuat akun administrator")
	}
	if certs != nil {
		log.Info("sapo-api siap", "versi", version, "https", cfg.API.TLS.Listen, "http_petunjuk", cfg.API.Listen,
			"sidik_jari_sha256", certs.fingerprint(), "jaringan_diizinkan", cfg.API.AllowedNetworks)
	} else {
		log.Warn("HTTPS mati: panel dan password lewat HTTP tanpa enkripsi (hanya untuk pengembangan)")
		log.Info("sapo-api siap", "versi", version, "alamat", cfg.API.Listen, "jaringan_diizinkan", cfg.API.AllowedNetworks)
	}

	errc := make(chan error, len(servers))
	for _, srv := range servers {
		go func(srv *http.Server) {
			var err error
			if srv.TLSConfig != nil {
				err = srv.ListenAndServeTLS("", "")
			} else {
				err = srv.ListenAndServe()
			}
			errc <- err
		}(srv)
	}
	for range servers {
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server berhenti", "err", err)
			os.Exit(1)
		}
	}
	log.Info("sapo-api berhenti")
}
