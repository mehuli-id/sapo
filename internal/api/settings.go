package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// --- Control Panel -----------------------------------------------------------

// Settings adalah pengaturan sistem dari Control Panel yang disimpan
// sapo-api (<data_dir>/settings.json). Nilai kosong = bawaan (layanan
// menyala).
type Settings struct {
	SMBOff bool `json:"smb_off,omitempty"`
	NFSOff bool `json:"nfs_off,omitempty"`
	// Sapo Drive (ADR 0013): pool folder pribadi (kosong = mati), kuota
	// bawaan dalam GiB (0 = tanpa batas), dan kuota per pengguna (-1 = tanpa
	// batas).
	HomesPool    string           `json:"homes_pool,omitempty"`
	HomesQuotaGB int64            `json:"homes_quota_gb,omitempty"`
	HomeQuotaGB  map[string]int64 `json:"home_quota_gb,omitempty"`
}

type settingsStore struct {
	path string
	mu   sync.Mutex
	cur  Settings
}

func openSettingsStore(dir string) (*settingsStore, error) {
	st := &settingsStore{path: filepath.Join(dir, "settings.json")}
	data, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &st.cur); err != nil {
		return nil, fmt.Errorf("file pengaturan rusak (%s): %w", st.path, err)
	}
	return st, nil
}

func (st *settingsStore) get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cur
}

// update mengubah pengaturan di bawah satu kunci (dua admin yang mengubah
// bersamaan tidak saling menghapus perubahan).
func (st *settingsStore) update(fn func(*Settings)) (Settings, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	v := st.cur
	fn(&v)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return st.cur, err
	}
	if err := writeFileAtomic(st.path, data); err != nil {
		return st.cur, err
	}
	st.cur = v
	return v, nil
}

func (s *Server) handleServices(w http.ResponseWriter, _ *http.Request) {
	v := s.settings.get()
	writeJSON(w, http.StatusOK, map[string]bool{"smb": !v.SMBOff, "nfs": !v.NFSOff})
}

// handleServicesUpdate menyalakan atau mematikan SMB dan NFS untuk semua
// shared folder. Mematikan memutus semua klien layanan itu.
func (s *Server) handleServicesUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SMB *bool `json:"smb"`
		NFS *bool `json:"nfs"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SMB == nil && req.NFS == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "tidak ada yang diubah")
		return
	}
	v, err := s.settings.update(func(v *Settings) {
		if req.SMB != nil {
			v.SMBOff = !*req.SMB
		}
		if req.NFS != nil {
			v.NFSOff = !*req.NFS
		}
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan pengaturan")
		return
	}
	warning := s.syncAfterChange(r)
	sess, _, _ := s.currentSession(r)
	s.record(r, sess.Username, "settings.services", warning == "", map[string]string{"smb": strconv.FormatBool(!v.SMBOff), "nfs": strconv.FormatBool(!v.NFSOff)})
	writeJSON(w, http.StatusOK, map[string]any{"smb": !v.SMBOff, "nfs": !v.NFSOff, "warning": warning})
}

func (s *Server) handleTimeSet(w http.ResponseWriter, r *http.Request) {
	var req proto.TimeSetParams
	if !decodeJSON(w, r, &req) {
		return
	}
	err := s.agent.Call(r.Context(), proto.OpTimeSet, req, nil)
	sess, _, _ := s.currentSession(r)
	s.record(r, sess.Username, "settings.time", err == nil, map[string]string{"timezone": auditText(req.Timezone), "ntp": strconv.FormatBool(req.NTP)})
	if err != nil {
		s.agentError(w, proto.OpTimeSet, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Pembaruan sistem (ADR 0011) ---------------------------------------------

func (s *Server) handleUpdatesCheck(w http.ResponseWriter, r *http.Request) {
	// apt-get update bisa lama: batas waktu sesuai sapod.
	timeout := time.Duration(s.cfg.Agent.Updates.CheckTimeoutSeconds+15) * time.Second
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + 15*time.Second))
	var out proto.UpdatesCheckResult
	if err := s.agent.Call(ctx, proto.OpUpdatesCheck, nil, &out); err != nil {
		s.agentError(w, proto.OpUpdatesCheck, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUpdatesInstall memasang pembaruan: mengubah seluruh sistem, jadi
// butuh password admin dan tercatat di log audit.
func (s *Server) handleUpdatesInstall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	err := s.agent.Call(r.Context(), proto.OpUpdatesInstall, nil, nil)
	detail := map[string]string{}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "updates.install", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpUpdatesInstall, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
