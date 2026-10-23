package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "tidak-ada.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Listen != "127.0.0.1:5000" {
		t.Fatalf("listen default = %q", cfg.API.Listen)
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sapo.yaml")
	if err := os.WriteFile(p, []byte("api:\n  listen: \"127.0.0.1:6000\"\nui:\n  default_mode: ringkas\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Listen != "127.0.0.1:6000" || cfg.UI.DefaultMode != "ringkas" {
		t.Fatalf("override tidak terbaca: %+v", cfg)
	}
	if cfg.Agent.Socket == "" {
		t.Fatal("default agent.socket hilang")
	}
}

func TestShippedConfigIsValid(t *testing.T) {
	if _, err := Load("../../config/sapo.yaml"); err != nil {
		t.Fatalf("config/sapo.yaml tidak valid: %v", err)
	}
}

func TestInvalidMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sapo.yaml")
	if err := os.WriteFile(p, []byte("ui:\n  default_mode: jendela\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("mode tidak valid seharusnya ditolak")
	}
}

func TestInvalidNetworkRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sapo.yaml")
	if err := os.WriteFile(p, []byte("api:\n  allowed_networks: [\"192.168.1.0/33\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("CIDR tidak valid harus ditolak")
	}
}

func TestWeakAuthPolicyRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sapo.yaml")
	if err := os.WriteFile(p, []byte("auth:\n  password_min_length: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("panjang password minimal di bawah 8 harus ditolak")
	}
}

func TestLayeredConfig(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "defaults.yaml")
	adm := filepath.Join(dir, "admin.yaml")
	_ = os.WriteFile(def, []byte("api:\n  listen: \":5000\"\nauth:\n  lockout_minutes: 20\n"), 0o644)
	_ = os.WriteFile(adm, []byte("api:\n  hostnames: [nas.kantor.lan]\n"), 0o644)
	cfg, err := LoadLayered(def, adm)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Listen != ":5000" || cfg.Auth.LockoutMinutes != 20 || len(cfg.API.Hostnames) != 1 {
		t.Fatalf("lapisan tidak tergabung: %+v %+v", cfg.API, cfg.Auth)
	}
	if cfg.Auth.LoginMaxAttempts != Default().Auth.LoginMaxAttempts {
		t.Fatal("nilai yang tidak disebut harus tetap default")
	}
}
