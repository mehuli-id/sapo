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

// Pemeriksaan pembaruan terjadwal (ADR 0011, tambahan): sapo-api meminta
// sapod memeriksa pembaruan (updates.check: apt-get update + simulasi)
// sekali sehari pada jam yang diatur admin, menyimpan hasilnya untuk Control
// Panel, dan memberi tahu notifier (kejadian updates_available). Pembaruan
// tidak pernah dipasang otomatis.

// UpdateCheckSettings: jadwal pemeriksaan otomatis (disimpan bersama hasil).
type UpdateCheckSettings struct {
	Off bool `json:"off,omitempty"`
	// Hour: jam pemeriksaan (waktu NAS); nil = bawaan konfigurasi.
	Hour *int `json:"hour,omitempty"`
}

type updateCheckFile struct {
	Settings UpdateCheckSettings `json:"settings"`
	// Last: hasil pemeriksaan berhasil terakhir (otomatis atau manual).
	Last *proto.UpdatesCheckResult `json:"last,omitempty"`
	// LastAttempt dan LastError: percobaan terakhir (galat bila gagal).
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

type updateChecks struct {
	path string
	mu   sync.Mutex
	cur  updateCheckFile
	// running: satu pemeriksaan pada satu waktu (manual atau terjadwal).
	running chan struct{}
}

func openUpdateChecks(dir string) (*updateChecks, error) {
	u := &updateChecks{path: filepath.Join(dir, "update-check.json"), running: make(chan struct{}, 1)}
	data, err := os.ReadFile(u.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &u.cur); err != nil {
			return nil, fmt.Errorf("file pemeriksaan pembaruan rusak (%s): %w", u.path, err)
		}
	}
	return u, nil
}

func (u *updateChecks) get() updateCheckFile {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cur
}

func (u *updateChecks) update(fn func(*updateCheckFile)) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	v := u.cur
	fn(&v)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(u.path, data); err != nil {
		return err
	}
	u.cur = v
	return nil
}

func (s *Server) updateHour(st UpdateCheckSettings) int {
	if st.Hour != nil {
		return *st.Hour
	}
	return s.cfg.API.UpdateCheck.DefaultHour
}

// runUpdateCheck menjalankan updates.check di sapod dan mencatat hasilnya.
// Menunggu pemeriksaan lain yang sedang berjalan (paling lama sampai ctx
// selesai).
func (s *Server) runUpdateCheck(ctx context.Context) (proto.UpdatesCheckResult, error) {
	var out proto.UpdatesCheckResult
	select {
	case s.updates.running <- struct{}{}:
	case <-ctx.Done():
		return out, &proto.Error{Code: proto.ErrBusy, Message: "pemeriksaan pembaruan lain sedang berjalan"}
	}
	defer func() { <-s.updates.running }()
	err := s.agent.Call(ctx, proto.OpUpdatesCheck, nil, &out)
	now := s.now().UTC()
	_ = s.updates.update(func(f *updateCheckFile) {
		f.LastAttempt = now
		if err != nil {
			f.LastError = clipText(err.Error(), 300)
			return
		}
		if out.Packages == nil {
			out.Packages = []proto.UpdatePackage{}
		}
		f.LastError, f.Last = "", &out
	})
	if err == nil {
		s.notify.wake()
	}
	return out, err
}

// updateCheckDue: jam pemeriksaan hari ini (waktu NAS) sudah lewat dan belum
// ada pemeriksaan berhasil sejak itu; yang gagal diulang paling cepat sejam
// kemudian. NAS yang mati pada jam itu memeriksa begitu menyala.
func updateCheckDue(f updateCheckFile, hour int, now time.Time, loc *time.Location) bool {
	if f.Settings.Off {
		return false
	}
	l := now.In(loc)
	start := time.Date(l.Year(), l.Month(), l.Day(), hour, 0, 0, 0, loc)
	if start.After(l) {
		start = start.AddDate(0, 0, -1)
	}
	if f.Last != nil && !f.Last.CheckedAt.Before(start) {
		return false
	}
	return f.LastAttempt.IsZero() || f.LastAttempt.Before(start) || now.Sub(f.LastAttempt) >= time.Hour
}

// nasLocation: zona waktu NAS dari sapod (bisa diubah dari panel setelah
// sapo-api berjalan).
func (s *Server) nasLocation(ctx context.Context) *time.Location {
	var ts proto.TimeStatus
	if err := s.agent.Call(ctx, proto.OpTimeStatus, nil, &ts); err == nil && ts.Timezone != "" {
		if loc, err := time.LoadLocation(ts.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

// UpdateCheckLoop memeriksa jadwal setiap 10 menit.
func (s *Server) UpdateCheckLoop(ctx context.Context) {
	for {
		f := s.updates.get()
		if !f.Settings.Off && !s.store.SetupRequired() {
			now := s.now()
			if updateCheckDue(f, s.updateHour(f.Settings), now, s.nasLocation(ctx)) {
				timeout := time.Duration(s.cfg.Agent.Updates.CheckTimeoutSeconds+15) * time.Second
				cctx, cancel := context.WithTimeout(ctx, timeout)
				if _, err := s.runUpdateCheck(cctx); err != nil && s.log != nil {
					s.log.Warn("pemeriksaan pembaruan terjadwal gagal", "err", err)
				}
				cancel()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
		}
	}
}

type updateCheckView struct {
	Off         bool                      `json:"off"`
	Hour        int                       `json:"hour"`
	Last        *proto.UpdatesCheckResult `json:"last,omitempty"`
	LastAttempt time.Time                 `json:"last_attempt,omitempty"`
	LastError   string                    `json:"last_error,omitempty"`
}

func (s *Server) handleUpdateCheckInfo(w http.ResponseWriter, _ *http.Request) {
	f := s.updates.get()
	writeJSON(w, http.StatusOK, updateCheckView{Off: f.Settings.Off, Hour: s.updateHour(f.Settings), Last: f.Last, LastAttempt: f.LastAttempt, LastError: f.LastError})
}

// handleUpdateCheckSet mengubah jadwal (hanya memeriksa, tidak memasang:
// tidak butuh password, tetap tercatat di audit).
func (s *Server) handleUpdateCheckSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Off  bool `json:"off"`
		Hour int  `json:"hour"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Hour < 0 || req.Hour > 23 {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "jam 0–23", map[string]any{"field": "hour"})
		return
	}
	err := s.updates.update(func(f *updateCheckFile) {
		h := req.Hour
		f.Settings = UpdateCheckSettings{Off: req.Off, Hour: &h}
	})
	sess, _, _ := s.currentSession(r)
	s.record(r, sess.Username, "updates.schedule", err == nil, map[string]string{"off": strconv.FormatBool(req.Off), "hour": strconv.Itoa(req.Hour)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan jadwal")
		return
	}
	s.notify.wake()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
