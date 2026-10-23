// sapod adalah System Agent Sapo. Ia berjalan sebagai root pada sistem
// terpasang, hanya mendengarkan unix socket lokal, dan hanya menjalankan
// operasi bernama yang terdaftar di sini.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mehuli-id/sapo/internal/agent"
	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/proto"
)

// version diisi saat build: -ldflags "-X main.version=..."
var version = "0.0.0-dev"

func main() {
	cfgPath := flag.String("config", config.DefaultPath, "lokasi sapo.yaml milik admin")
	defaultsPath := flag.String("defaults", "", "lokasi nilai bawaan paket (misalnya "+config.DefaultsPath+")")
	socket := flag.String("socket", "", "timpa agent.socket (untuk pengembangan)")
	featuresDir := flag.String("features", "", "timpa agent.features_dir (untuk pengembangan)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.LoadLayered(*defaultsPath, *cfgPath)
	if err != nil {
		log.Error("konfigurasi", "err", err)
		os.Exit(1)
	}
	if *socket != "" {
		cfg.Agent.Socket = *socket
	}
	if *featuresDir != "" {
		cfg.Agent.FeaturesDir = *featuresDir
	}

	reg := agent.NewRegistry()
	reg.Register(proto.OpSystemInfo, agent.SystemInfoOp(agent.DefaultProcFS, version))
	reg.Register(proto.OpFeaturesList, agent.FeaturesListOp(cfg.Agent.FeaturesDir))

	ln, err := agent.Listen(cfg.Agent.Socket, cfg.Agent.SocketGroup)
	if err != nil {
		log.Error("membuka socket", "socket", cfg.Agent.Socket, "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if os.Geteuid() != 0 {
		log.Warn("sapod berjalan tanpa root: hanya operasi baca yang akan berfungsi (mode pengembangan)")
	}
	log.Info("sapod siap", "versi", version, "socket", cfg.Agent.Socket, "operasi", reg.Ops())

	srv := &agent.Server{Registry: reg, Log: log, OpTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute, WriteTimeout: 30 * time.Second, MaxConns: cfg.Agent.MaxConnections}
	if err := srv.Serve(ctx, ln); err != nil {
		log.Error("server berhenti", "err", err)
		os.Exit(1)
	}
	_ = os.Remove(cfg.Agent.Socket)
	log.Info("sapod berhenti")
}
