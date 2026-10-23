// Package config memuat konfigurasi Sapo dari sapo.yaml.
//
// Semua nilai punya default yang aman. Nilai di file menimpa default,
// dan pengaturan dari panel nantinya menulis ke file/database yang sama.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultPath adalah lokasi konfigurasi pada sistem terpasang.
const DefaultPath = "/etc/sapo/sapo.yaml"

// Config adalah konfigurasi lengkap Sapo.
type Config struct {
	Agent Agent `yaml:"agent"`
	API   API   `yaml:"api"`
	UI    UI    `yaml:"ui"`
}

// Agent mengatur sapod.
type Agent struct {
	Socket      string `yaml:"socket"`
	SocketGroup string `yaml:"socket_group"`
	FeaturesDir string `yaml:"features_dir"`
}

// API mengatur sapo-api.
type API struct {
	Listen             string `yaml:"listen"`
	WebDir             string `yaml:"web_dir"`
	BrandingFile       string `yaml:"branding_file"`
	DataDir            string `yaml:"data_dir"`
	DefaultLanguage    string `yaml:"default_language"`
	SessionIdleMinutes int    `yaml:"session_idle_minutes"`
}

// UI mengatur default tampilan panel.
type UI struct {
	DefaultMode string `yaml:"default_mode"`
}

// Default mengembalikan konfigurasi bawaan yang aman (hanya localhost).
func Default() Config {
	return Config{
		Agent: Agent{
			Socket:      "/run/sapo/sapod.sock",
			SocketGroup: "sapo",
			FeaturesDir: "/usr/share/sapo/features",
		},
		API: API{
			Listen:             "127.0.0.1:5000",
			WebDir:             "/usr/share/sapo/web",
			BrandingFile:       "/usr/share/sapo/branding.yaml",
			DataDir:            "/var/lib/sapo",
			DefaultLanguage:    "id",
			SessionIdleMinutes: 30,
		},
		UI: UI{DefaultMode: "desktop"},
	}
}

// Load membaca file konfigurasi di path. Bila file tidak ada, default dipakai.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("membaca %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("format %s tidak valid: %w", path, err)
	}
	return cfg, cfg.Validate()
}

// Validate memeriksa nilai yang wajib ada.
func (c Config) Validate() error {
	switch {
	case c.Agent.Socket == "":
		return errors.New("agent.socket wajib diisi")
	case c.API.Listen == "":
		return errors.New("api.listen wajib diisi")
	case c.UI.DefaultMode != "desktop" && c.UI.DefaultMode != "ringkas":
		return fmt.Errorf("ui.default_mode harus desktop atau ringkas, bukan %q", c.UI.DefaultMode)
	}
	return nil
}
