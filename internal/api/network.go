package api

import (
	"encoding/json"
	"net/http"

	"github.com/mehuli-id/sapo/internal/proto"
)

// --- Pengaturan jaringan (ADR 0009) ------------------------------------------

func (s *Server) handleNetworkApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Config        proto.NetworkConfig `json:"config"`
		AdminPassword string              `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// Salah atur jaringan bisa memutus NAS dari semua komputer: password
	// admin, log audit, dan konfirmasi dari alamat baru (sapod).
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	var out proto.NetworkApplyResult
	err := s.agent.Call(r.Context(), proto.OpNetworkApply, proto.NetworkApplyParams{Config: req.Config}, &out)
	detail := map[string]string{"hostname": auditText(req.Config.Hostname), "config": networkText(req.Config)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "network.apply", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpNetworkApply, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func networkText(c proto.NetworkConfig) string {
	b, _ := json.Marshal(c)
	s := string(b)
	if len(s) > 1000 {
		s = s[:1000] + "…"
	}
	return s
}

func (s *Server) handleNetworkToken(op, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req proto.NetworkTokenParams
		if !decodeJSON(w, r, &req) {
			return
		}
		if len(req.Token) != 64 {
			writeError(w, http.StatusBadRequest, "bad_request", "token tidak valid")
			return
		}
		err := s.agent.Call(r.Context(), op, req, nil)
		// Hanya yang berhasil dicatat: tanpa login, siapa pun di jaringan
		// yang diizinkan bisa mengirim token salah, dan itu tidak boleh
		// membanjiri log audit.
		if err == nil {
			s.record(r, "", action, true, nil)
		}
		if err != nil {
			s.agentError(w, op, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
