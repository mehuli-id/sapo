package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/mehuli-id/sapo/internal/proto"
)

// --- Penyimpanan eksternal (USB, ADR 0015) ------------------------------------

func (s *Server) handleExternalList(w http.ResponseWriter, r *http.Request) {
	var out []proto.ExternalVolume
	if err := s.agent.Call(r.Context(), proto.OpExternalList, nil, &out); err != nil {
		s.agentError(w, proto.OpExternalList, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// externalNames: nama folder eksternal yang diatur (terpasang atau tidak).
func (s *Server) externalNames(r *http.Request) (map[string]bool, error) {
	var vols []proto.ExternalVolume
	if err := s.agent.Call(r.Context(), proto.OpExternalList, nil, &vols); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, v := range vols {
		if v.Name != "" {
			out[v.Name] = true
		}
	}
	return out, nil
}

// handleExternalMount memasang drive USB sebagai folder eksternal: memberi
// akses ke data di drive, jadi butuh password admin dan tercatat di log
// audit.
func (s *Server) handleExternalMount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		proto.ExternalMountParams
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(strings.ToLower(req.Name))
	switch {
	case !shareNameRe.MatchString(req.Name) || slices.Contains(reservedShareNames, req.Name):
		writeErrorDetail(w, http.StatusBadRequest, "share_invalid", "nama folder tidak sesuai aturan", map[string]any{"field": "name"})
		return
	case s.shareExists(req.Name):
		writeErrorDetail(w, http.StatusConflict, "share_taken", "nama sudah dipakai shared folder", map[string]any{"field": "name"})
		return
	case req.Group != allUsersGroup && !s.groupExists(req.Group):
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "grup tidak ditemukan", map[string]any{"field": "group"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	err := s.agent.Call(r.Context(), proto.OpExternalMount, req.ExternalMountParams, nil)
	detail := map[string]string{"name": req.Name, "uuid": auditText(req.UUID), "group": auditText(req.Group),
		"read_only": strconv.FormatBool(req.ReadOnly), "smb": strconv.FormatBool(req.SMB)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "external.mount", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpExternalMount, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleExternalUnmount melepas drive dengan aman (data ditulis tuntas).
func (s *Server) handleExternalUnmount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.agent.Call(r.Context(), proto.OpExternalUnmount, proto.ExternalParams{Name: name}, nil)
	sess, _, _ := s.currentSession(r)
	detail := map[string]string{"name": auditText(name)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, sess.Username, "external.unmount", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpExternalUnmount, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
