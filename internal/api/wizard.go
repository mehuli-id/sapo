package api

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Wizard adalah definisi wizard sebagai data (wizards/<id>.yaml): langkah,
// isian, default, dan petunjuk dalam beberapa bahasa. Panel merender wizard
// dari data ini, jadi wizard baru tidak butuh kode UI baru.
type Wizard struct {
	ID     string            `yaml:"id" json:"id"`
	Title  map[string]string `yaml:"title" json:"title"`
	Action string            `yaml:"action" json:"action"`
	// FinishLabel: teks tombol terakhir (bawaan panel: Selesai).
	FinishLabel map[string]string `yaml:"finish_label" json:"finish_label,omitempty"`
	// Destructive: tindakan akhir menghapus data (tombol terakhir berwarna bahaya).
	Destructive bool         `yaml:"destructive" json:"destructive,omitempty"`
	Steps       []WizardStep `yaml:"steps" json:"steps"`
}

// WizardStep adalah satu langkah wizard.
type WizardStep struct {
	ID          string            `yaml:"id" json:"id"`
	Type        string            `yaml:"type" json:"type,omitempty"`
	Title       map[string]string `yaml:"title" json:"title"`
	Description map[string]string `yaml:"description" json:"description,omitempty"`
	Help        map[string]string `yaml:"help" json:"help,omitempty"`
	Fields      []WizardField     `yaml:"fields" json:"fields,omitempty"`
}

// WizardField adalah satu isian.
type WizardField struct {
	Key         string            `yaml:"key" json:"key"`
	Type        string            `yaml:"type" json:"type"`
	Label       map[string]string `yaml:"label" json:"label"`
	Help        map[string]string `yaml:"help" json:"help,omitempty"`
	Placeholder map[string]string `yaml:"placeholder" json:"placeholder,omitempty"`
	Required    bool              `yaml:"required" json:"required,omitempty"`
	Default     string            `yaml:"default" json:"default,omitempty"`
	Match       string            `yaml:"match" json:"match,omitempty"`
	// Source: isian lain yang dipakai jenis isian ini (pool-layout dan
	// erase-confirm membaca disk dari isian disk-select).
	Source string `yaml:"source" json:"source,omitempty"`
	// Pattern: pola isian teks untuk petunjuk di panel; server tetap
	// memeriksa sendiri.
	Pattern      string `yaml:"pattern" json:"pattern,omitempty"`
	Autocomplete string `yaml:"autocomplete" json:"autocomplete,omitempty"`
	Secret       bool   `yaml:"secret" json:"secret,omitempty"`
}

var wizardID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Jenis yang dikenal renderer panel. Jenis lain ditolak agar kesalahan di
// file data ketahuan saat dimuat, bukan saat pengguna sedang mengisi.
var (
	stepTypes  = map[string]bool{"": true, "form": true, "review": true}
	fieldTypes = map[string]bool{
		"text": true, "username": true, "password": true, "password-confirm": true, "current-password": true,
		"language": true, "totp-enroll": true, "recovery-codes": true,
		"disk-select": true, "pool-layout": true, "erase-confirm": true, "confirm": true,
	}
	// Jenis isian yang wajib merujuk isian disk-select lewat source.
	needsDiskSource = map[string]bool{"pool-layout": true, "erase-confirm": true}
)

// LoadWizard membaca dan memvalidasi wizard dari dir.
func LoadWizard(dir, id string) (*Wizard, error) {
	if !wizardID.MatchString(id) {
		return nil, fs.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".yaml"))
	if err != nil {
		return nil, err
	}
	var wz Wizard
	if err := yaml.Unmarshal(data, &wz); err != nil {
		return nil, err
	}
	if wz.ID != id || len(wz.Steps) == 0 {
		return nil, fmt.Errorf("wizard %s: id tidak cocok atau tidak ada langkah", id)
	}
	keys := map[string]bool{}
	for _, st := range wz.Steps {
		if !stepTypes[st.Type] {
			return nil, fmt.Errorf("wizard %s: jenis langkah tidak dikenal %q", id, st.Type)
		}
		for _, f := range st.Fields {
			if !fieldTypes[f.Type] {
				return nil, fmt.Errorf("wizard %s: jenis isian tidak dikenal %q", id, f.Type)
			}
			if keys[f.Key] {
				return nil, fmt.Errorf("wizard %s: kunci isian ganda %q", id, f.Key)
			}
			keys[f.Key] = true
		}
	}
	types := map[string]string{}
	for _, st := range wz.Steps {
		for _, f := range st.Fields {
			types[f.Key] = f.Type
		}
	}
	for _, st := range wz.Steps {
		for _, f := range st.Fields {
			if f.Match != "" && !keys[f.Match] {
				return nil, fmt.Errorf("wizard %s: isian %q merujuk %q yang tidak ada", id, f.Key, f.Match)
			}
			if (f.Type == "confirm" || f.Type == "password-confirm") && f.Match == "" {
				return nil, fmt.Errorf("wizard %s: isian %q butuh match", id, f.Key)
			}
			if needsDiskSource[f.Type] && types[f.Source] != "disk-select" {
				return nil, fmt.Errorf("wizard %s: isian %q butuh source berupa isian disk-select", id, f.Key)
			}
			if f.Pattern != "" {
				if _, err := regexp.Compile(f.Pattern); err != nil {
					return nil, fmt.Errorf("wizard %s: pola isian %q tidak valid: %w", id, f.Key, err)
				}
			}
		}
	}
	return &wz, nil
}

func (s *Server) handleWizard(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Wizard setup boleh dibaca tanpa login (dibutuhkan sebelum ada akun);
	// wizard lain hanya untuk pengguna yang sudah login.
	if id != "setup" {
		if _, _, ok := s.currentSession(r); !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "silakan login")
			return
		}
	}
	wz, err := LoadWizard(s.cfg.API.WizardsDir, id)
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "not_found", "wizard tidak ada")
		return
	}
	if err != nil {
		if s.log != nil {
			s.log.Error("wizard tidak valid", "id", id, "err", err)
		}
		writeError(w, http.StatusInternalServerError, "wizard_invalid", "definisi wizard tidak valid")
		return
	}
	writeJSON(w, http.StatusOK, wz)
}
