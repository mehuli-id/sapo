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
		"disks": strings.Join(names, ","), "erase": strings.Join(erase, ","),
	}
	// Dicatat sebelum disk disentuh. Nilai dari pengguna (nama pool, nama
	// disk) disimpan sebagai data JSON, jadi tidak bisa memalsukan baris lain.
	if err := s.audit.Record(audit.Event{Time: s.now().UTC(), Actor: user.Username, Action: "storage.pool_create.request", IP: clientIP(r), OK: true, Detail: detail}); err != nil {
		if s.log != nil {
			s.log.Error("log audit tidak bisa ditulis; pool tidak dibuat", "err", err)
		}
		writeError(w, http.StatusInternalServerError, "audit_unavailable", "log audit tidak bisa ditulis")
		return
	}

	// Membuat pool bisa lebih lama dari batas tulis server HTTP.
	timeout := time.Duration(s.cfg.Agent.Storage.Pools.CreateTimeoutSeconds)*time.Second + 15*time.Second
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + 15*time.Second))
	// Tidak dibatalkan bila browser menutup koneksi: sapod tetap
	// menyelesaikan pembuatan pool, jadi hasil di log audit harus hasil nyata.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
	defer cancel()
	var pool json.RawMessage
	err := s.agent.Call(ctx, proto.OpStoragePoolCreate, proto.PoolCreateParams{Name: req.Name, Layout: req.Layout, Disks: req.Disks}, &pool)
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, user.Username, "storage.pool_create", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpStoragePoolCreate, err)
		return
	}
	writeRaw(w, pool)
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
	err := s.agent.Call(r.Context(), proto.OpStoragePoolScrub, params, nil)
	detail := map[string]string{"pool": params.Name, "action": params.Action}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	// Nama pool dari URL hanya dicatat bila bentuknya wajar.
	if len(params.Name) > 64 {
		detail["pool"] = params.Name[:64] + "…"
	}
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
