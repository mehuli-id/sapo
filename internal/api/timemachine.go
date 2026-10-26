package api

import (
	"net/http"
	"strconv"

	"github.com/mehuli-id/sapo/internal/proto"
)

// handleShareTimeMachine menjadikan folder tujuan Time Machine Mac (ADR
// 0022) dengan kuota, atau mengembalikannya menjadi folder biasa (kuota
// dilepas; backup Mac yang ada tetap di folder). Tempat sampah folder itu
// dimatikan: Time Machine menghapus banyak file setiap backup.
func (s *Server) handleShareTimeMachine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled       bool   `json:"enabled"`
		QuotaGB       int    `json:"quota_gb"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Enabled && (req.QuotaGB < proto.MinTimeMachineGB || req.QuotaGB > proto.MaxTimeMachineGB) {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "kuota Time Machine minimal 10 GB", map[string]any{"field": "quota_gb"})
		return
	}
	name := r.PathValue("name")
	if _, ok := s.shares.get(name); !ok {
		writeError(w, http.StatusNotFound, "not_found", "shared folder tidak ditemukan")
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	if err := s.shares.mutate(func(cur []Share) ([]Share, error) {
		for i := range cur {
			if cur[i].Name != name {
				continue
			}
			if req.Enabled {
				cur[i].TimeMachine = &proto.TimeMachine{QuotaGB: req.QuotaGB}
				cur[i].RecycleDays = 0
			} else {
				cur[i].TimeMachine = nil
			}
		}
		return cur, nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan shared folder")
		return
	}
	warning := s.syncAfterChange(r)
	detail := map[string]string{"share": name, "enabled": strconv.FormatBool(req.Enabled)}
	if req.Enabled {
		detail["quota_gb"] = strconv.Itoa(req.QuotaGB)
	}
	s.record(r, admin.Username, "shares.time_machine", warning == "", detail)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
}
