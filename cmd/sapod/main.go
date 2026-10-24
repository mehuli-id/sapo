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
	sc := cfg.Agent.Storage
	storage := agent.NewStorage(agent.StorageTools{
		Lsblk:          sc.Lsblk,
		Smartctl:       sc.Smartctl,
		Timeout:        time.Duration(sc.CommandTimeoutSeconds) * time.Second,
		IgnoreDevices:  sc.IgnoreDevices,
		MaxConcurrent:  sc.MaxConcurrentCommands,
		CacheFor:       time.Duration(sc.SMARTCacheSeconds) * time.Second,
		WarnATARawIDs:  sc.WarnATARawIDs,
		WarnNVMeFields: sc.WarnNVMeFields,
	})
	reg.Register(proto.OpStorageDisks, storage.DisksOp())
	reg.Register(proto.OpStorageSMART, storage.SMARTOp())
	pc := sc.Pools
	layouts := make([]proto.PoolLayout, 0, len(pc.Layouts))
	for _, l := range pc.Layouts {
		layouts = append(layouts, proto.PoolLayout{ID: l.ID, MinDisks: l.MinDisks, MaxDisks: l.MaxDisks})
	}
	pools := agent.NewPools(storage, agent.PoolTools{
		Zpool:          pc.Zpool,
		MountBase:      pc.MountBase,
		Ashift:         pc.Ashift,
		RootProperties: pc.RootProperties,
		Layouts:        layouts,
		AllowRemovable: pc.AllowRemovable,
		CreateTimeout:  time.Duration(pc.CreateTimeoutSeconds) * time.Second,
		Log:            log,
	})
	reg.Register(proto.OpStoragePools, pools.PoolsOp())
	reg.Register(proto.OpStoragePoolCreate, pools.PoolCreateOp())
	reg.Register(proto.OpStoragePoolScrub, pools.PoolScrubOp())
	reg.Register(proto.OpStoragePoolReplace, pools.PoolReplaceOp())
	reg.Register(proto.OpStoragePoolDestroy, pools.PoolDestroyOp())
	ac := cfg.Agent.Accounts
	accounts := agent.NewAccounts(agent.AccountTools{
		Useradd: ac.Useradd, Userdel: ac.Userdel, Groupadd: ac.Groupadd, Groupdel: ac.Groupdel, Gpasswd: ac.Gpasswd,
		Smbpasswd: ac.Smbpasswd, Smbstatus: ac.Smbstatus, PasswdFile: ac.PasswdFile, GroupFile: ac.GroupFile, StateFile: ac.StateFile,
		IDMin: ac.IDMin, IDMax: ac.IDMax, PrimaryGroup: ac.PrimaryGroup, Shell: ac.Shell,
		Timeout: time.Duration(sc.CommandTimeoutSeconds) * time.Second, Log: log,
	})
	reg.Register(proto.OpAccountsSync, accounts.SyncOp())
	reg.Register(proto.OpSMBPassword, accounts.SMBPasswordOp())
	shc := cfg.Agent.Shares
	shares := agent.NewShares(pools, agent.ShareTools{
		Zfs: shc.Zfs, Setfacl: shc.Setfacl, Testparm: shc.Testparm, Smbcontrol: shc.Smbcontrol, SMBConf: shc.SMBConf,
		Workgroup: shc.Workgroup, ServerString: shc.ServerString, MinProtocol: shc.MinProtocol,
		MountBase: pc.MountBase, PasswdFile: ac.PasswdFile, GroupFile: ac.GroupFile,
		Exportfs: shc.Exportfs, Systemctl: shc.Systemctl, ExportsFile: shc.ExportsFile, NFSService: shc.NFSService,
		IDMin: ac.IDMin, IDMax: ac.IDMax,
	})
	reg.Register(proto.OpSharesApply, shares.ApplyOp())
	reg.Register(proto.OpSharesDelete, shares.DeleteOp())
	reg.Register(proto.OpSharesUsage, shares.UsageOp())
	reg.Register(proto.OpConsolePanelAddress, agent.ConsolePanelAddressOp(agent.ConsoleTools{
		IssueFile: cfg.Agent.Console.IssueFile,
		Agetty:    cfg.Agent.Console.Agetty,
		Timeout:   10 * time.Second,
	}))

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
