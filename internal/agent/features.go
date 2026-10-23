package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/mehuli-id/sapo/internal/proto"
	"gopkg.in/yaml.v3"
)

var (
	featureIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// Nama paket Debian: huruf kecil, angka, dan + - . (Debian Policy 5.6.1).
	packagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	// Nama unit systemd yang diizinkan muncul di manifest.
	servicePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9@._-]{0,127}$`)
)

// LoadFeatures membaca semua manifest *.yaml di dir dan memvalidasinya.
// Manifest adalah data: tidak ada daftar paket yang ditulis di kode.
func LoadFeatures(dir string) ([]proto.Feature, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]proto.Feature, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var f proto.Feature
		if err := yaml.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		if err := validateFeature(f); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		if seen[f.ID] {
			return nil, fmt.Errorf("%s: id %q dipakai dua kali", filepath.Base(p), f.ID)
		}
		seen[f.ID] = true
		out = append(out, f)
	}
	for _, f := range out {
		for _, r := range f.Requires {
			if !seen[r] {
				return nil, fmt.Errorf("fitur %q membutuhkan %q yang tidak ada", f.ID, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func validateFeature(f proto.Feature) error {
	if !featureIDPattern.MatchString(f.ID) {
		return fmt.Errorf("id fitur tidak valid: %q", f.ID)
	}
	if f.Name["id"] == "" || f.Name["en"] == "" {
		return fmt.Errorf("fitur %q wajib punya nama id dan en", f.ID)
	}
	switch f.Activation {
	case "always", "setup", "optional":
	default:
		return fmt.Errorf("fitur %q: activation harus always, setup, atau optional", f.ID)
	}
	for _, p := range f.Packages {
		if !packagePattern.MatchString(p) {
			return fmt.Errorf("nama paket tidak valid di %q: %q", f.ID, p)
		}
	}
	for _, s := range f.Services {
		if !servicePattern.MatchString(s) {
			return fmt.Errorf("nama layanan tidak valid di %q: %q", f.ID, s)
		}
	}
	return nil
}

// FeaturesListOp mengembalikan handler features.list (hanya membaca manifest).
func FeaturesListOp(dir string) Handler {
	return func(_ context.Context, _ json.RawMessage) (any, error) {
		features, err := LoadFeatures(dir)
		if err != nil {
			return nil, &OpError{Code: proto.ErrInternal, Message: "manifest fitur: " + err.Error()}
		}
		return features, nil
	}
}
