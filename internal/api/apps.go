package api

import (
	"net/http"
	"strconv"

	"github.com/mehuli-id/sapo/internal/proto"
)

// --- App Center (ADR 0012) ---------------------------------------------------

// handleAppInstall memasang aplikasi dari katalog: menjalankan program baru
// di NAS, jadi butuh password admin dan tercatat di log audit.
func (s *Server) handleAppInstall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		proto.AppInstallParams
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	err := s.poolCall(r.Context(), proto.OpAppsInstall, req.AppInstallParams, nil)
	detail := map[string]string{"app": auditText(req.ID), "pool": auditText(req.Pool), "port": strconv.Itoa(req.Port)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "apps.install", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpAppsInstall, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAppToggle menjalankan atau menghentikan aplikasi.
func (s *Server) handleAppToggle(op, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := s.agent.Call(r.Context(), op, proto.AppParams{ID: id}, nil)
		sess, _, _ := s.currentSession(r)
		s.record(r, sess.Username, action, err == nil, map[string]string{"app": auditText(id)})
		if err != nil {
			s.agentError(w, op, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// handleAppRemove melepas aplikasi. Menghapus datanya juga destruktif: nama
// diketik ulang dan password admin.
func (s *Server) handleAppRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RemoveData    bool   `json:"remove_data"`
		ConfirmName   string `json:"confirm_name"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	sess, _, _ := s.currentSession(r)
	actor := sess.Username
	if req.RemoveData {
		if req.ConfirmName != id {
			writeErrorDetail(w, http.StatusBadRequest, "confirm_mismatch", "nama yang diketik ulang tidak sama", map[string]any{"field": "confirm_name"})
			return
		}
		admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
		if !ok {
			return
		}
		actor = admin.Username
	}
	err := s.poolCall(r.Context(), proto.OpAppsRemove, proto.AppRemoveParams{ID: id, RemoveData: req.RemoveData}, nil)
	detail := map[string]string{"app": auditText(id), "remove_data": strconv.FormatBool(req.RemoveData)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, actor, "apps.remove", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpAppsRemove, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
