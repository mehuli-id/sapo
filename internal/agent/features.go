package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mehuli-id/sapo/internal/proto"
	"gopkg.in/yaml.v3"
)

var (
	featureIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	groupPattern     = featureIDPattern
	// Nama paket Debian (Debian Policy 5.6.1), dengan syarat tambahan agar
	// tidak ambigu bagi apt: tidak diakhiri "-" (apt membacanya sebagai
	// "hapus paket") dan tidak memuat ".+" / ".*" (dibaca sebagai regex).
	packagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*[a-z0-9+]$`)
	// Unit systemd: nama dasar, opsional dengan akhiran .service/.socket/.timer.
	servicePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9@_-]{0,120}(\.(service|socket|timer))?$`)
)

// Unit yang tidak boleh dikendalikan lewat manifest fitur.
var forbiddenServices = map[string]bool{"sapod": true, "sapo-api": true}

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
	byID := map[string]proto.Feature{}
	for _, f := range out {
		byID[f.ID] = f
		for _, r := range f.Requires {
			if !seen[r] {
				return nil, fmt.Errorf("fitur %q membutuhkan %q yang tidak ada", f.ID, r)
			}
		}
	}
	if cyc := findCycle(byID); cyc != "" {
		return nil, fmt.Errorf("ketergantungan fitur melingkar: %s", cyc)
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
	if !groupPattern.MatchString(f.Group) {
		return fmt.Errorf("fitur %q: grup tidak valid %q", f.ID, f.Group)
	}
	for _, p := range f.Packages {
		if !packagePattern.MatchString(p) || strings.Contains(p, ".+") || strings.Contains(p, ".*") {
			return fmt.Errorf("nama paket tidak valid di %q: %q", f.ID, p)
		}
	}
	for _, s := range f.Services {
		base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, ".service"), ".socket"), ".timer")
		if !servicePattern.MatchString(s) || forbiddenServices[base] {
			return fmt.Errorf("nama layanan tidak valid di %q: %q", f.ID, s)
		}
	}
	return nil
}

// findCycle mencari ketergantungan melingkar (A butuh B, B butuh A).
func findCycle(byID map[string]proto.Feature) string {
	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	var path []string
	var visit func(id string) bool
	visit = func(id string) bool {
		switch state[id] {
		case visiting:
			path = append(path, id)
			return true
		case done:
			return false
		}
		state[id] = visiting
		path = append(path, id)
		for _, r := range byID[id].Requires {
			if visit(r) {
				return true
			}
		}
		path = path[:len(path)-1]
		state[id] = done
		return false
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if visit(id) {
			return strings.Join(path, " → ")
		}
	}
	return ""
}

// FeaturesListOp mengembalikan handler features.list (hanya membaca manifest).
func FeaturesListOp(dir string) Handler {
	return func(_ context.Context, _ json.RawMessage) (any, error) {
		features, err := LoadFeatures(dir)
		if err != nil {
			// Detailnya dicatat di log sapod oleh dispatch; klien hanya tahu gagal.
			return nil, fmt.Errorf("manifest fitur: %w", err)
		}
		return features, nil
	}
}
