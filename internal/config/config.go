// Package config memuat konfigurasi Sapo dari sapo.yaml.
//
// Semua nilai punya default yang aman. Nilai di file menimpa default,
// dan pengaturan dari panel nantinya menulis ke file/database yang sama.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/mehuli-id/sapo/internal/proto"
)

// DefaultPath adalah lokasi konfigurasi pada sistem terpasang.
const DefaultPath = "/etc/sapo/sapo.yaml"

// Config adalah konfigurasi lengkap Sapo.
type Config struct {
	Agent Agent `yaml:"agent"`
	API   API   `yaml:"api"`
	Auth  Auth  `yaml:"auth"`
	UI    UI    `yaml:"ui"`
}

// Agent mengatur sapod.
type Agent struct {
	Socket      string `yaml:"socket"`
	SocketGroup string `yaml:"socket_group"`
	FeaturesDir string `yaml:"features_dir"`
	// Batas koneksi bersamaan ke socket sapod.
	MaxConnections int     `yaml:"max_connections"`
	Storage        Storage `yaml:"storage"`
	Console        Console `yaml:"console"`
}

// Console mengatur alamat panel di layar login konsol NAS. File ditulis sapod
// (root) di direktori miliknya; /etc/issue.d/sapo.issue dari paket menautkan
// ke sini. agetty dipakai untuk menampilkan ulang layar login.
type Console struct {
	IssueFile string `yaml:"issue_file"`
	Agetty    string `yaml:"agetty"`
}

// Storage mengatur operasi baca disk di sapod.
type Storage struct {
	// Lokasi program (path absolut, tidak dicari lewat PATH).
	Lsblk    string `yaml:"lsblk"`
	Smartctl string `yaml:"smartctl"`
	// Batas waktu satu pemanggilan program, dalam detik.
	CommandTimeoutSeconds int `yaml:"command_timeout_seconds"`
	// Pola nama perangkat (glob) yang tidak ditampilkan sebagai disk.
	IgnoreDevices []string `yaml:"ignore_devices"`
	// Program yang boleh berjalan bersamaan; permintaan lain menunggu.
	MaxConcurrentCommands int `yaml:"max_concurrent_commands"`
	// Lama hasil SMART (dan daftar disk untuk validasi) dipakai ulang.
	SMARTCacheSeconds int `yaml:"smart_cache_seconds"`
	// Atribut ATA (ID) dan kolom log kesehatan NVMe yang memicu status
	// peringatan bila nilai mentahnya bukan nol.
	WarnATARawIDs  []int    `yaml:"warn_ata_raw_ids"`
	WarnNVMeFields []string `yaml:"warn_nvme_fields"`
	Pools          Pools    `yaml:"pools"`
}

// Pools mengatur pool ZFS: cara sapod membuat pool dan susunan yang boleh
// dipilih di panel.
type Pools struct {
	Zpool string `yaml:"zpool"`
	// Pool dipasang di <mount_base>/<nama pool>.
	MountBase string `yaml:"mount_base"`
	// ashift 12 = sektor 4 KiB; aman juga untuk disk bersektor 512 byte.
	Ashift int `yaml:"ashift"`
	// Properti dataset akar setiap pool baru (zpool create -O).
	RootProperties map[string]string `yaml:"root_properties"`
	Layouts        []PoolLayout      `yaml:"layouts"`
	// Disk lepas-pasang (flashdisk, kartu memori) tidak cocok untuk pool.
	AllowRemovable       bool `yaml:"allow_removable"`
	CreateTimeoutSeconds int  `yaml:"create_timeout_seconds"`
}

// PoolLayout adalah satu susunan pool yang boleh dipilih beserta batas
// jumlah disknya. Arti setiap id (single, mirror, raidz1-3) tetap di kode.
type PoolLayout struct {
	ID       string `yaml:"id"`
	MinDisks int    `yaml:"min_disks"`
	MaxDisks int    `yaml:"max_disks"`
}

// API mengatur sapo-api.
type API struct {
	Listen          string `yaml:"listen"`
	WebDir          string `yaml:"web_dir"`
	BrandingFile    string `yaml:"branding_file"`
	WizardsDir      string `yaml:"wizards_dir"`
	DataDir         string `yaml:"data_dir"`
	DefaultLanguage string `yaml:"default_language"`
	// Jaringan yang boleh membuka panel. Default: localhost dan LAN privat.
	// Akses dari internet diatur terpisah lewat wizard "Akses dari internet".
	// Daftar ini bukan batas autentikasi: login tetap wajib.
	AllowedNetworks []string `yaml:"allowed_networks"`
	// Jaringan yang selalu ditolak walau masuk allowed_networks, misalnya
	// jaringan container Docker (aplikasi App Center tidak boleh membuka panel).
	DeniedNetworks []string `yaml:"denied_networks"`
	// Nama host tambahan yang boleh dipakai membuka panel (selain alamat IP,
	// localhost, nama host mesin, dan <nama host>.local). Mencegah DNS rebinding.
	Hostnames []string `yaml:"hostnames"`
	// Batas ukuran log audit sebelum dirotasi (MB).
	AuditMaxMB int `yaml:"audit_max_mb"`
	TLS        TLS `yaml:"tls"`
}

// TLS mengatur HTTPS panel di jaringan lokal (ADR 0004).
type TLS struct {
	// Bila aktif, panel dan API hanya dilayani lewat HTTPS; alamat HTTP
	// (api.listen) hanya menampilkan petunjuk pindah ke HTTPS.
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	// Masa berlaku sertifikat self-signed dan kapan diperbarui (hari).
	ValidityDays    int `yaml:"validity_days"`
	RenewBeforeDays int `yaml:"renew_before_days"`
}

// Auth berisi kebijakan login. Semua angka di sini bisa diubah admin.
type Auth struct {
	PasswordMinLength   int      `yaml:"password_min_length"`
	UsernamePattern     string   `yaml:"username_pattern"`
	ReservedUsernames   []string `yaml:"reserved_usernames"`
	RequireTOTPForAdmin bool     `yaml:"require_totp_for_admin"`
	SessionIdleMinutes  int      `yaml:"session_idle_minutes"`
	SessionMaxHours     int      `yaml:"session_max_hours"`
	LoginMaxAttempts    int      `yaml:"login_max_attempts"`
	LoginWindowMinutes  int      `yaml:"login_window_minutes"`
	LockoutMinutes      int      `yaml:"lockout_minutes"`
	// Batas kode 2FA salah per akun (terpisah dari batas password).
	TOTPMaxFailures        int `yaml:"totp_max_failures"`
	TOTPWindowMinutes      int `yaml:"totp_window_minutes"`
	SetupTOTPExpiryMinutes int `yaml:"setup_totp_expiry_minutes"`
	// Jumlah kode cadangan 2FA yang dibuat, dan batas sisa yang memicu
	// peringatan di panel.
	RecoveryCodeCount int `yaml:"recovery_code_count"`
	RecoveryCodesLow  int `yaml:"recovery_codes_low"`
	// Batas hash password yang berjalan bersamaan (Argon2 memakai banyak RAM).
	MaxConcurrentHashes int    `yaml:"max_concurrent_hashes"`
	Argon2              Argon2 `yaml:"argon2"`
}

// Argon2 adalah parameter hash password.
type Argon2 struct {
	MemoryKiB   uint32 `yaml:"memory_kib"`
	Iterations  uint32 `yaml:"iterations"`
	Parallelism uint8  `yaml:"parallelism"`
}

// UI mengatur default tampilan panel.
type UI struct {
	DefaultMode string `yaml:"default_mode"`
}

// Default mengembalikan konfigurasi bawaan yang aman.
func Default() Config {
	return Config{
		Agent: Agent{
			Socket:         "/run/sapo/sapod.sock",
			SocketGroup:    "sapo",
			FeaturesDir:    "/usr/share/sapo/features",
			MaxConnections: 32,
			Console:        Console{IssueFile: "/run/sapo/console.issue", Agetty: "/usr/sbin/agetty"},
			Storage: Storage{
				Lsblk:                 "/usr/bin/lsblk",
				Smartctl:              "/usr/sbin/smartctl",
				CommandTimeoutSeconds: 20,
				IgnoreDevices:         []string{"zram*", "ram*", "loop*", "fd*"},
				MaxConcurrentCommands: 2,
				SMARTCacheSeconds:     60,
				WarnATARawIDs:         []int{5, 187, 197, 198},
				WarnNVMeFields:        []string{"media_errors", "critical_warning"},
				Pools: Pools{
					Zpool:     "/usr/sbin/zpool",
					MountBase: "/srv/sapo",
					Ashift:    12,
					RootProperties: map[string]string{
						"compression": "lz4", "atime": "off", "xattr": "sa", "acltype": "posixacl",
					},
					Layouts: []PoolLayout{
						{ID: proto.LayoutSingle, MinDisks: 1, MaxDisks: 1},
						{ID: proto.LayoutMirror, MinDisks: 2, MaxDisks: 4},
						{ID: proto.LayoutRAIDZ1, MinDisks: 3, MaxDisks: 8},
						{ID: proto.LayoutRAIDZ2, MinDisks: 4, MaxDisks: 12},
						{ID: proto.LayoutRAIDZ3, MinDisks: 5, MaxDisks: 16},
					},
					CreateTimeoutSeconds: 120,
				},
			},
		},
		API: API{
			Listen:          "127.0.0.1:5000",
			WebDir:          "/usr/share/sapo/web",
			BrandingFile:    "/usr/share/sapo/branding.yaml",
			WizardsDir:      "/usr/share/sapo/wizards",
			DataDir:         "/var/lib/sapo",
			DefaultLanguage: "id",
			AllowedNetworks: []string{
				"127.0.0.0/8", "::1/128", // localhost
				"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // LAN IPv4
				"fc00::/7", "fe80::/10", // LAN IPv6
			},
			DeniedNetworks: []string{"172.17.0.0/16"}, // bridge Docker bawaan
			AuditMaxMB:     10,
			TLS: TLS{
				Enabled:         true,
				Listen:          ":5001",
				ValidityDays:    397,
				RenewBeforeDays: 30,
			},
		},
		Auth: Auth{
			PasswordMinLength:      10,
			UsernamePattern:        `^[a-z][a-z0-9._-]{2,31}$`,
			ReservedUsernames:      []string{"root", "sapo", "daemon", "bin", "sys", "nobody", "guest"},
			RequireTOTPForAdmin:    true,
			SessionIdleMinutes:     30,
			SessionMaxHours:        12,
			LoginMaxAttempts:       5,
			LoginWindowMinutes:     15,
			LockoutMinutes:         15,
			TOTPMaxFailures:        10,
			TOTPWindowMinutes:      60,
			SetupTOTPExpiryMinutes: 15,
			RecoveryCodeCount:      10,
			RecoveryCodesLow:       3,
			MaxConcurrentHashes:    2,
			Argon2:                 Argon2{MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 2},
		},
		UI: UI{DefaultMode: "desktop"},
	}
}

// DefaultsPath adalah lokasi nilai bawaan yang dipasang paket.
const DefaultsPath = "/usr/share/sapo/sapo.yaml"

// Load membaca satu file konfigurasi di atas nilai bawaan program.
func Load(path string) (Config, error) {
	return LoadLayered("", path)
}

// LoadLayered membaca konfigurasi berlapis: nilai bawaan program, lalu file
// bawaan paket (defaults, ikut diperbarui saat upgrade), lalu file admin
// (path, hanya berisi nilai yang diubah admin atau panel). File yang tidak
// ada dilewati.
func LoadLayered(defaults, path string) (Config, error) {
	cfg := Default()
	for _, p := range []string{defaults, path} {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return cfg, fmt.Errorf("membaca %s: %w", p, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("format %s tidak valid: %w", p, err)
		}
	}
	return cfg, cfg.Validate()
}

// Validate memeriksa nilai yang wajib ada dan masuk akal.
func (c Config) Validate() error {
	switch {
	case c.Agent.Socket == "":
		return errors.New("agent.socket wajib diisi")
	case c.Agent.MaxConnections < 1:
		return errors.New("agent.max_connections minimal 1")
	case !filepath.IsAbs(c.Agent.Console.IssueFile) || !filepath.IsAbs(c.Agent.Console.Agetty):
		return errors.New("agent.console: issue_file dan agetty harus path absolut")
	case !filepath.IsAbs(c.Agent.Storage.Lsblk) || !filepath.IsAbs(c.Agent.Storage.Smartctl):
		return errors.New("agent.storage: lsblk dan smartctl harus path absolut")
	case c.Agent.Storage.CommandTimeoutSeconds < 1 || c.Agent.Storage.CommandTimeoutSeconds > 25:
		// sapo-api menunggu sapod paling lama 30 detik.
		return errors.New("agent.storage.command_timeout_seconds harus 1 sampai 25")
	case c.Agent.Storage.MaxConcurrentCommands < 1 || c.Agent.Storage.MaxConcurrentCommands > 16:
		return errors.New("agent.storage.max_concurrent_commands harus 1 sampai 16")
	case c.Agent.Storage.SMARTCacheSeconds < 0:
		return errors.New("agent.storage.smart_cache_seconds tidak boleh negatif")
	case c.API.Listen == "":
		return errors.New("api.listen wajib diisi")
	case c.UI.DefaultMode != "desktop" && c.UI.DefaultMode != "ringkas":
		return fmt.Errorf("ui.default_mode harus desktop atau ringkas, bukan %q", c.UI.DefaultMode)
	case len(c.API.AllowedNetworks) == 0:
		return errors.New("api.allowed_networks tidak boleh kosong")
	case c.Auth.PasswordMinLength < 8:
		return errors.New("auth.password_min_length minimal 8")
	case c.Auth.LoginMaxAttempts < 1 || c.Auth.LockoutMinutes < 1 || c.Auth.LoginWindowMinutes < 1:
		return errors.New("auth: batas percobaan login, jendela, dan lockout harus lebih dari 0")
	case c.Auth.SessionIdleMinutes < 1 || c.Auth.SessionMaxHours < 1:
		return errors.New("auth: durasi sesi harus lebih dari 0")
	case c.Auth.TOTPMaxFailures < 1 || c.Auth.TOTPWindowMinutes < 1:
		return errors.New("auth: totp_max_failures dan totp_window_minutes harus lebih dari 0")
	case c.API.TLS.ValidityDays < 30 || c.API.TLS.ValidityDays > 825:
		// Apple menolak sertifikat server TLS yang berlaku lebih dari 825 hari.
		return errors.New("api.tls.validity_days harus 30 sampai 825")
	case c.API.TLS.RenewBeforeDays < 1 || c.API.TLS.RenewBeforeDays >= c.API.TLS.ValidityDays:
		return errors.New("api.tls.renew_before_days harus antara 1 dan validity_days")
	case c.API.AuditMaxMB < 1:
		return errors.New("api.audit_max_mb minimal 1")
	case c.Auth.MaxConcurrentHashes < 1:
		return errors.New("auth.max_concurrent_hashes minimal 1")
	case c.Auth.RecoveryCodeCount < 4 || c.Auth.RecoveryCodeCount > 20:
		return errors.New("auth.recovery_code_count harus 4 sampai 20")
	case c.Auth.RecoveryCodesLow < 0 || c.Auth.RecoveryCodesLow >= c.Auth.RecoveryCodeCount:
		return errors.New("auth.recovery_codes_low harus 0 sampai kurang dari recovery_code_count")
	case c.Auth.SetupTOTPExpiryMinutes < 1:
		return errors.New("auth.setup_totp_expiry_minutes harus lebih dari 0")
	case c.Auth.Argon2.MemoryKiB < 8*1024 || c.Auth.Argon2.Iterations < 1 || c.Auth.Argon2.Parallelism < 1:
		return errors.New("auth.argon2: memory_kib minimal 8192, iterations dan parallelism minimal 1")
	}
	if _, err := regexp.Compile(c.Auth.UsernamePattern); err != nil {
		return fmt.Errorf("auth.username_pattern tidak valid: %w", err)
	}
	if _, err := ListenPort("api.listen", c.API.Listen); err != nil {
		return err
	}
	if c.API.TLS.Enabled {
		tlsPort, err := ListenPort("api.tls.listen", c.API.TLS.Listen)
		if err != nil {
			return err
		}
		if httpPort, _ := ListenPort("api.listen", c.API.Listen); httpPort == tlsPort {
			return errors.New("api.tls.listen harus memakai port yang berbeda dari api.listen")
		}
	}
	for _, h := range c.API.Hostnames {
		if !hostnamePattern.MatchString(h) {
			return fmt.Errorf("api.hostnames: %q bukan nama host yang valid (huruf kecil a-z, angka, titik, tanda hubung; tanpa port)", h)
		}
	}
	if err := c.Agent.Storage.Pools.validate(); err != nil {
		return err
	}
	for _, p := range c.Agent.Storage.IgnoreDevices {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("agent.storage.ignore_devices: pola %q tidak valid", p)
		}
	}
	if _, err := c.Networks(); err != nil {
		return err
	}
	if _, err := parsePrefixes("api.denied_networks", c.API.DeniedNetworks); err != nil {
		return err
	}
	return nil
}

// hostnamePattern: nama DNS ASCII huruf kecil tanpa port atau wildcard, agar
// selalu bisa dimasukkan ke sertifikat dan dicocokkan dengan header Host.
var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// ListenPort mengambil nomor port dari alamat listen ("host:port" atau ":port").
func ListenPort(name, addr string) (int, error) {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("%s: %q bukan alamat host:port", name, addr)
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%s: port %q tidak valid", name, p)
	}
	return n, nil
}

// Networks mengurai api.allowed_networks.
func (c Config) Networks() ([]netip.Prefix, error) {
	return parsePrefixes("api.allowed_networks", c.API.AllowedNetworks)
}

// Denied mengurai api.denied_networks.
func (c Config) Denied() ([]netip.Prefix, error) {
	return parsePrefixes("api.denied_networks", c.API.DeniedNetworks)
}

func parsePrefixes(name string, list []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %q bukan CIDR yang valid", name, s)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// Properti ZFS dari konfigurasi diteruskan ke zpool create apa adanya, jadi
// bentuknya dibatasi: nama properti (termasuk properti pengguna modul:nama)
// dan nilai tanpa spasi atau tanda sama dengan.
var (
	zfsPropName  = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,63}$`)
	zfsPropValue = regexp.MustCompile(`^[A-Za-z0-9_.:@+-]{1,128}$`)
)

func (p Pools) validate() error {
	switch {
	case !filepath.IsAbs(p.Zpool):
		return errors.New("agent.storage.pools.zpool harus path absolut")
	case !filepath.IsAbs(p.MountBase) || filepath.Clean(p.MountBase) != p.MountBase || p.MountBase == "/":
		return errors.New("agent.storage.pools.mount_base harus path absolut yang rapi dan bukan /")
	case p.Ashift < 9 || p.Ashift > 16:
		return errors.New("agent.storage.pools.ashift harus 9 sampai 16")
	case p.CreateTimeoutSeconds < 10 || p.CreateTimeoutSeconds > 240:
		return errors.New("agent.storage.pools.create_timeout_seconds harus 10 sampai 240 (batas operasi sapod 5 menit)")
	case len(p.Layouts) == 0:
		return errors.New("agent.storage.pools.layouts tidak boleh kosong")
	}
	for k, v := range p.RootProperties {
		if !zfsPropName.MatchString(k) || !zfsPropValue.MatchString(v) {
			return fmt.Errorf("agent.storage.pools.root_properties: %q=%q tidak valid", k, v)
		}
	}
	seen := map[string]bool{}
	for _, l := range p.Layouts {
		floor, known := proto.LayoutFloor[l.ID]
		switch {
		case !known:
			return fmt.Errorf("agent.storage.pools.layouts: susunan %q tidak dikenal", l.ID)
		case seen[l.ID]:
			return fmt.Errorf("agent.storage.pools.layouts: susunan %q ditulis dua kali", l.ID)
		case l.MinDisks < floor:
			return fmt.Errorf("agent.storage.pools.layouts: %s butuh min_disks minimal %d", l.ID, floor)
		case l.MaxDisks < l.MinDisks || l.MaxDisks > 64:
			return fmt.Errorf("agent.storage.pools.layouts: %s: max_disks harus antara min_disks dan 64", l.ID)
		case l.ID == proto.LayoutSingle && l.MaxDisks != 1:
			return errors.New("agent.storage.pools.layouts: single selalu satu disk (max_disks 1)")
		}
		seen[l.ID] = true
	}
	return nil
}
