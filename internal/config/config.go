// Package config memuat konfigurasi Sapo dari sapo.yaml.
//
// Semua nilai punya default yang aman. Nilai di file menimpa default,
// dan pengaturan dari panel nantinya menulis ke file/database yang sama.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
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
	MaxConnections int `yaml:"max_connections"`
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
	case c.API.AuditMaxMB < 1:
		return errors.New("api.audit_max_mb minimal 1")
	case c.Auth.MaxConcurrentHashes < 1:
		return errors.New("auth.max_concurrent_hashes minimal 1")
	case c.Auth.SetupTOTPExpiryMinutes < 1:
		return errors.New("auth.setup_totp_expiry_minutes harus lebih dari 0")
	case c.Auth.Argon2.MemoryKiB < 8*1024 || c.Auth.Argon2.Iterations < 1 || c.Auth.Argon2.Parallelism < 1:
		return errors.New("auth.argon2: memory_kib minimal 8192, iterations dan parallelism minimal 1")
	}
	if _, err := regexp.Compile(c.Auth.UsernamePattern); err != nil {
		return fmt.Errorf("auth.username_pattern tidak valid: %w", err)
	}
	if _, err := c.Networks(); err != nil {
		return err
	}
	if _, err := parsePrefixes("api.denied_networks", c.API.DeniedNetworks); err != nil {
		return err
	}
	return nil
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
