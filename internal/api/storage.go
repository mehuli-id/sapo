package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mehuli-id/sapo/internal/audit"
	"github.com/mehuli-id/sapo/internal/auth"
	"github.com/mehuli-id/sapo/internal/proto"
)

// --- Pool ZFS (ADR 0005) -----------------------------------------------------
//
// Membuat pool menghapus isi disk, jadi sapo-api meminta tiga hal sebelum
// meneruskannya ke sapod: password (sesi yang dicuri saja tidak cukup), nama
// pool diketik ulang, dan konfirmasi penghapusan per disk berisi data (yang
// diperiksa lagi oleh sapod). Permintaan dicatat di log audit sebelum disk
// disentuh; bila log audit tidak bisa ditulis, pool tidak dibuat.

// requireAdmin: hanya administrator. Peran dibaca dari data akun, bukan dari
// sesi, agar perubahan peran langsung berlaku.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := r.Context().Value(sessionKey).(auth.Session)
		if u, ok := s.store.Get(sess.Username); !ok || u.Role != auth.RoleAdmin {
			writeError(w, http.StatusForbidden, "forbidden", "hanya administrator")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

type poolCreateRequest struct {
	Name        string          `json:"name"`
	Layout      string          `json:"layout"`
	Disks       []proto.DiskRef `json:"disks"`
	ConfirmName string          `json:"confirm_name"`
	Password    string          `json:"password"`
	// AllowUSB: admin sengaja memakai disk USB (ADR 0015).
	AllowUSB bool `json:"allow_usb"`
}

// maxPoolDisks membatasi isi permintaan; batas sebenarnya per susunan ada di
// konfigurasi dan diperiksa sapod.
const maxPoolDisks = 64

func (s *Server) handlePoolCreate(w http.ResponseWriter, r *http.Request) {
	var req poolCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Disks) == 0 || len(req.Disks) > maxPoolDisks {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "pilih disk untuk pool", map[string]any{"field": "disks"})
		return
	}
	if req.ConfirmName != req.Name {
		writeErrorDetail(w, http.StatusBadRequest, "confirm_mismatch", "nama pool yang diketik ulang tidak sama", map[string]any{"field": "confirm_name"})
		return
	}
	user, ok := s.reauth(w, r, req.Password)
	if !ok {
		return
	}

	names, erase := make([]string, 0, len(req.Disks)), []string{}
	for _, d := range req.Disks {
		names = append(names, d.Name)
		if d.Erase {
			erase = append(erase, d.Name)
		}
	}
	detail := map[string]string{
		"pool": req.Name, "layout": req.Layout,
		"disks": strings.Join(names, ","), "erase": strings.Join(erase, ","), "allow_usb": strconv.FormatBool(req.AllowUSB),
	}
	s.destructive(w, r, user, "storage.pool_create", detail, proto.OpStoragePoolCreate,
		proto.PoolCreateParams{Name: req.Name, Layout: req.Layout, Disks: req.Disks, AllowUSB: req.AllowUSB}, nil)
}

// destructive menjalankan operasi sapod yang menghapus data, setelah
// password diperiksa: permintaan dicatat di log audit lebih dulu (bila
// gagal, operasi dibatalkan), operasi tidak ikut batal bila browser menutup
// koneksi, lalu hasil nyatanya dicatat.
// after (boleh nil) dijalankan setelah operasi berhasil, sebelum jawaban.
// poolCall menjalankan operasi pool setelah sinkronisasi akun dan shared
// folder yang sedang berjalan selesai. Keduanya memakai kunci pool yang sama
// di sapod, jadi tanpa ini operasi pool ditolak "busy" hanya karena
// sinkronisasi latar belakang (misalnya saat sapo-api baru menyala).
func (s *Server) poolCall(ctx context.Context, op string, params, out any) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	return s.agent.Call(ctx, op, params, out)
}

func (s *Server) destructive(w http.ResponseWriter, r *http.Request, user auth.User, action string, detail map[string]string, op string, params any, after func()) {
	// Nilai dari pengguna (nama pool, nama disk) disimpan sebagai data JSON,
	// jadi tidak bisa memalsukan baris lain.
	if err := s.audit.Record(audit.Event{Time: s.now().UTC(), Actor: user.Username, Action: action + ".request", IP: clientIP(r), OK: true, Detail: detail}); err != nil {
		if s.log != nil {
			s.log.Error("log audit tidak bisa ditulis; operasi dibatalkan", "aksi", action, "err", err)
		}
		writeError(w, http.StatusInternalServerError, "audit_unavailable", "log audit tidak bisa ditulis")
		return
	}
	// Operasi pool bisa lebih lama dari batas tulis server HTTP.
	timeout := time.Duration(s.cfg.Agent.Storage.Pools.CreateTimeoutSeconds)*time.Second + 15*time.Second
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + 15*time.Second))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
	defer cancel()
	var out json.RawMessage
	err := s.poolCall(ctx, op, params, &out)
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, user.Username, action, err == nil, detail)
	if err != nil {
		s.agentError(w, op, err)
		return
	}
	if after != nil {
		after()
	}
	writeRaw(w, out)
}

func (s *Server) handlePoolReplace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldGUID  string        `json:"old_guid"`
		Disk     proto.DiskRef `json:"disk"`
		Password string        `json:"password"`
		AllowUSB bool          `json:"allow_usb"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	user, ok := s.reauth(w, r, req.Password)
	if !ok {
		return
	}
	name := r.PathValue("name")
	detail := map[string]string{"pool": auditText(name), "old_guid": auditText(req.OldGUID), "disk": auditText(req.Disk.Name), "erase": strconv.FormatBool(req.Disk.Erase), "allow_usb": strconv.FormatBool(req.AllowUSB)}
	s.destructive(w, r, user, "storage.pool_replace", detail, proto.OpStoragePoolReplace,
		proto.PoolReplaceParams{Name: name, OldGUID: req.OldGUID, Disk: req.Disk, AllowUSB: req.AllowUSB}, nil)
}

func (s *Server) handlePoolDestroy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ConfirmName string `json:"confirm_name"`
		Password    string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if req.ConfirmName != name {
		writeErrorDetail(w, http.StatusBadRequest, "confirm_mismatch", "nama pool yang diketik ulang tidak sama", map[string]any{"field": "confirm_name"})
		return
	}
	user, ok := s.reauth(w, r, req.Password)
	if !ok {
		return
	}
	s.destructive(w, r, user, "storage.pool_destroy", map[string]string{"pool": auditText(name)}, proto.OpStoragePoolDestroy,
		// Folder di pool yang dihapus tidak lagi dibagikan (Samba dan NFS).
		proto.PoolDestroyParams{Name: name}, func() {
			// Folder pribadi (Sapo Drive) ikut terhapus bersama pool: Sapo
			// Drive dimatikan agar bisa dinyalakan lagi di pool lain.
			if s.settings.get().HomesPool == name {
				_, _ = s.settings.update(func(v *Settings) { v.HomesPool = "" })
			}
			s.syncAfterChange(r)
		})
}

// auditText memotong nilai dari pengguna sebelum dicatat.
func auditText(v string) string {
	if len(v) > 64 {
		return v[:64] + "…"
	}
	return v
}

func (s *Server) handlePoolScrub(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := r.Context().Value(sessionKey).(auth.Session)
	params := proto.PoolScrubParams{Name: r.PathValue("name"), Action: req.Action}
	err := s.poolCall(r.Context(), proto.OpStoragePoolScrub, params, nil)
	detail := map[string]string{"pool": params.Name, "action": params.Action}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	detail["pool"] = auditText(params.Name)
	s.record(r, sess.Username, "storage.pool_scrub", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpStoragePoolScrub, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// errorCode untuk log audit: kode sapod, atau agent_unavailable.
func errorCode(err error) string {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return "agent_unavailable"
}

func writeRaw(w http.ResponseWriter, raw json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
