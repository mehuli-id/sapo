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
	// Proses pembantu File Station (dijalankan sapod sendiri sebagai akun
	// pengguna): satu operasi file, tanpa konfigurasi.
	if len(os.Args) > 1 && os.Args[1] == "fs-helper" {
		os.Exit(agent.FSHelperMain(os.Stdin, os.Stdout))
	}
	// Pemasangan pembaruan (dijalankan systemd-run oleh sapod sendiri).
	if len(os.Args) > 1 && os.Args[1] == "apt-run" {
		os.Exit(agent.AptRunMain(os.Args[2:]))
	}
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
	timeTools := agent.TimeTools{Timedatectl: cfg.Agent.Time.Timedatectl, Timeout: 20 * time.Second}
	reg.Register(proto.OpTimeStatus, agent.TimeStatusOp(timeTools))
	reg.Register(proto.OpTimeZones, agent.TimeZonesOp(timeTools))
	reg.Register(proto.OpTimeSet, agent.TimeSetOp(timeTools))
	stc := cfg.Agent.Stats
	stats := agent.NewStats(agent.StatsTools{
		ProcStat: "/proc/stat", MemInfo: "/proc/meminfo", NetDev: "/proc/net/dev", DiskStats: "/proc/diskstats",
		SysNet: "/sys/class/net", SysBlock: "/sys/block",
		Interval: time.Duration(stc.IntervalSeconds) * time.Second, History: stc.History,
		IdleStop: time.Duration(stc.IdleStopSeconds) * time.Second, IgnoreDevices: cfg.Agent.Storage.IgnoreDevices,
	})
	reg.Register(proto.OpSystemStats, stats.StatsOp())
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
		Exportfs: shc.Exportfs, Systemctl: shc.Systemctl, ExportsFile: shc.ExportsFile, NFSService: shc.NFSService, SMBService: shc.SMBService,
		DenyNetworks: []string{cfg.Agent.Apps.Subnet},
		IDMin:        ac.IDMin, IDMax: ac.IDMax,
	})
	shares.SetAccounts(accounts)
	self, err := os.Executable()
	if err != nil {
		log.Error("lokasi sapod", "err", err)
		os.Exit(1)
	}
	fc := cfg.Agent.Files
	files := agent.NewFiles(agent.FilesTools{
		Helper: self, MountBase: pc.MountBase, GroupFile: ac.GroupFile, MaxEntries: fc.MaxEntries,
		Timeout: time.Duration(fc.TimeoutSeconds) * time.Second, DeleteTimeout: time.Duration(fc.DeleteTimeoutSeconds) * time.Second,
		IDMin: ac.IDMin, IDMax: ac.IDMax,
	}, shares, accounts.Managed)
	apc := cfg.Agent.Apps
	apps := agent.NewApps(agent.AppsTools{
		CatalogDir: apc.CatalogDir, Podman: apc.Podman, QuadletDir: apc.QuadletDir, StateFile: apc.StateFile,
		Systemctl: shc.Systemctl, Useradd: ac.Useradd, Userdel: ac.Userdel, Groupadd: ac.Groupadd, Groupdel: ac.Groupdel,
		IDMin: apc.IDMin, IDMax: apc.IDMax, ReservedPorts: apc.ReservedPorts, MountBase: pc.MountBase,
		Timeout: time.Minute, Log: log,
	}, shares)
	// File Quadlet disamakan dengan katalog (paket Sapo bisa membawa digest
	// baru), di latar belakang.
	go apps.Reconcile(context.Background())
	reg.Register(proto.OpAppsCatalog, apps.CatalogOp())
	reg.Register(proto.OpAppsList, apps.ListOp())
	reg.Register(proto.OpAppsInstall, apps.InstallOp())
	reg.Register(proto.OpAppsStart, apps.StartOp())
	reg.Register(proto.OpAppsStop, apps.StopOp())
	reg.Register(proto.OpAppsRemove, apps.RemoveOp())
	uc := cfg.Agent.Updates
	updates := agent.NewUpdates(agent.UpdatesTools{
		AptGet: uc.AptGet, Dpkg: uc.Dpkg, SystemdRun: uc.SystemdRun, Systemctl: cfg.Agent.Shares.Systemctl, Self: self,
		Unit: uc.Unit, LogFile: uc.LogFile, ResultFile: uc.ResultFile,
		CheckTimeout:   time.Duration(uc.CheckTimeoutSeconds) * time.Second,
		InstallTimeout: time.Duration(uc.InstallTimeoutSeconds) * time.Second,
	})
	reg.Register(proto.OpUpdatesCheck, updates.CheckOp())
	reg.Register(proto.OpUpdatesInstall, updates.InstallOp())
	reg.Register(proto.OpUpdatesStatus, updates.StatusOp())
	reg.Register(proto.OpFilesList, files.ListOp())
	reg.Register(proto.OpFilesStat, files.StatOp())
	reg.Register(proto.OpFilesMkdir, files.MkdirOp())
	reg.Register(proto.OpFilesRename, files.RenameOp())
	reg.Register(proto.OpFilesDelete, files.DeleteOp())
	reg.Register(proto.OpFilesOpenRead, files.OpenReadOp())
	reg.Register(proto.OpFilesOpenWrite, files.OpenWriteOp())
	reg.Register(proto.OpFilesCommit, files.CommitOp())
	reg.Register(proto.OpFilesAbort, files.AbortOp())
	reg.Register(proto.OpSharesApply, shares.ApplyOp())
	reg.Register(proto.OpSharesDelete, shares.DeleteOp())
	reg.Register(proto.OpSharesUsage, shares.UsageOp())
	snc := cfg.Agent.Snapshots
	snapshots := agent.NewSnapshots(shares, agent.SnapshotTools{
		Zpool: pc.Zpool, MaxPoolCapacity: snc.MaxPoolCapacityPercent, CallTimeout: time.Duration(snc.CallTimeoutSeconds) * time.Second,
	})
	go snapshots.Run(context.Background())
	reg.Register(proto.OpSnapshotsList, snapshots.ListOp())
	reg.Register(proto.OpSnapshotsCreate, snapshots.CreateOp())
	reg.Register(proto.OpSnapshotsDelete, snapshots.DeleteOp())
	homes := agent.NewHomes(shares, accounts.Managed)
	reg.Register(proto.OpHomesApply, homes.ApplyOp())
	reg.Register(proto.OpHomesStatus, homes.StatusOp())
	reg.Register(proto.OpHomesDelete, homes.DeleteOp())
	nc := cfg.Agent.Network
	network := agent.NewNetwork(agent.NetworkTools{
		IP: nc.IP, Systemctl: nc.Systemctl, Hostnamectl: nc.Hostnamectl,
		InterfacesFile: nc.InterfacesFile, ResolvConf: nc.ResolvConf, HostsFile: nc.HostsFile, HostnameFile: nc.HostnameFile, NetDir: nc.NetDir,
		StateFile: nc.StateFile, PendingFile: nc.PendingFile, NetworkService: nc.NetworkService, AvahiService: nc.AvahiService,
		ApplyDelay: time.Duration(nc.ApplyDelaySeconds) * time.Second, ConfirmWindow: time.Duration(nc.ConfirmSeconds) * time.Second,
		Timeout: time.Minute, Log: log,
	})
	// Perubahan jaringan yang belum dikonfirmasi saat sapod berhenti
	// dikembalikan sebelum sapod menerima operasi apa pun.
	network.Recover(context.Background())
	reg.Register(proto.OpNetworkStatus, network.StatusOp())
	reg.Register(proto.OpNetworkApply, network.ApplyOp())
	reg.Register(proto.OpNetworkConfirm, network.ConfirmOp())
	reg.Register(proto.OpNetworkCancel, network.CancelOp())
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
