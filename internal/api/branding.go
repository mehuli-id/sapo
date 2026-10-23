package api

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// branding adalah bagian branding.yaml yang dikirim ke panel. Bagian
// "updates" (repo & kunci rilis) sengaja tidak ikut.
type branding struct {
	Product struct {
		Name     string            `yaml:"name" json:"name"`
		FullName string            `yaml:"full_name" json:"full_name"`
		Vendor   string            `yaml:"vendor" json:"vendor"`
		Website  string            `yaml:"website" json:"website"`
		Tagline  map[string]string `yaml:"tagline" json:"tagline"`
	} `yaml:"product" json:"product"`
	Theme struct {
		Radius string            `yaml:"radius" json:"radius,omitempty"`
		Light  map[string]string `yaml:"light" json:"light,omitempty"`
		Dark   map[string]string `yaml:"dark" json:"dark,omitempty"`
	} `yaml:"theme" json:"theme"`
}

var (
	cssValue = regexp.MustCompile(`^[a-zA-Z0-9 .,%()/#-]{1,64}$`)
	cssName  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
)

func loadBranding(path string) (*branding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b branding
	if err := yaml.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	// Nilai tema disisipkan sebagai CSS di panel: hanya izinkan karakter aman.
	check := func(m map[string]string) error {
		for k, v := range m {
			if !cssName.MatchString(k) || !cssValue.MatchString(v) {
				return fmt.Errorf("nilai tema tidak valid: %s", k)
			}
		}
		return nil
	}
	if err := check(b.Theme.Light); err != nil {
		return nil, err
	}
	if err := check(b.Theme.Dark); err != nil {
		return nil, err
	}
	if b.Theme.Radius != "" && !cssValue.MatchString(b.Theme.Radius) {
		return nil, fmt.Errorf("radius tidak valid")
	}
	if w := b.Product.Website; w != "" && !strings.HasPrefix(w, "https://") && !strings.HasPrefix(w, "http://") {
		return nil, fmt.Errorf("product.website harus http(s)")
	}
	return &b, nil
}

// productName dipakai sebagai "issuer" di aplikasi autentikator.
func (s *Server) productName() string {
	if b, err := loadBranding(s.cfg.API.BrandingFile); err == nil && b.Product.Name != "" {
		return b.Product.Name
	}
	return "Sapo"
}
