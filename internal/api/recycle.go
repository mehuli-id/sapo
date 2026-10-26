package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Tempat sampah shared folder (ADR 0021). Lama simpan disimpan bersama
// folder di shares.json dan diteruskan ke sapod lewat shares.apply; isi
// tempat sampah dibersihkan dan dihitung oleh sapod.

func (s *Server) handleRecycle(w http.ResponseWriter, r *http.Request) {
	var out []proto.RecycleUsage
	if err := s.agent.Call(r.Context(), proto.OpRecycleStatus, nil, &out); err != nil {
		s.agentError(w, proto.OpRecycleStatus, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleShareRecycle mengatur lama simpan (0 = mati; isi yang ada tidak
// dihapus). Butuh password admin: lama simpan yang lebih pendek (atau
// menyalakan lagi) membuang isi yang lebih lama.
func (s *Server) handleShareRecycle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days          int    `json:"days"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Days < 0 || req.Days > proto.MaxRecycleDays {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "lama simpan tempat sampah 0–365 hari", map[string]any{"field": "days"})
		return
	}
	name := r.PathValue("name")
	sh, ok := s.shares.get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "shared folder tidak ditemukan")
		return
	}
	if sh.TimeMachine != nil && req.Days > 0 {
		writeErrorDetail(w, http.StatusConflict, "time_machine_share", "folder Time Machine tidak memakai tempat sampah", map[string]any{"field": "days"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	if err := s.shares.mutate(func(cur []Share) ([]Share, error) {
		for i := range cur {
			if cur[i].Name == name {
				cur[i].RecycleDays = req.Days
			}
		}
		return cur, nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan shared folder")
		return
	}
	warning := s.syncAfterChange(r)
	s.record(r, admin.Username, "recycle.policy", warning == "", map[string]string{"share": name, "days": strconv.Itoa(req.Days), "before": strconv.Itoa(sh.RecycleDays)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
}

// handleShareRecycleEmpty menghapus permanen seluruh isi tempat sampah satu
// folder: nama folder diketik ulang, password admin, dan audit.
func (s *Server) handleShareRecycleEmpty(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ConfirmName   string `json:"confirm_name"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	sh, ok := s.shares.get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "shared folder tidak ditemukan")
		return
	}
	if req.ConfirmName != name {
		writeErrorDetail(w, http.StatusBadRequest, "confirm_mismatch", "nama yang diketik ulang tidak sama", map[string]any{"field": "confirm_name"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5*time.Minute + 10*time.Second))
	var out proto.RecycleEmptyResult
	err := s.agent.Call(ctx, proto.OpRecycleEmpty, proto.RecycleTarget{Pool: sh.Pool, Share: sh.Name}, &out)
	detail := map[string]string{"share": name, "removed": strconv.Itoa(out.Removed), "done": strconv.FormatBool(out.Done)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "recycle.empty", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpRecycleEmpty, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
