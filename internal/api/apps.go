package api

import (
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

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
	// Akses folder sisa pemasangan lama (misalnya penghapusan yang gagal di
	// tengah jalan) tidak ikut ke pemasangan baru: harus diberikan ulang.
	warning := s.purgeAppFolders(r, admin.Username, req.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
}

// purgeAppFolders mencabut semua akses folder aplikasi id (dicatat di log
// audit bila ada yang dicabut).
func (s *Server) purgeAppFolders(r *http.Request, actor, id string) string {
	var revoked []string
	err := s.shares.mutate(func(cur []Share) ([]Share, error) {
		for i := range cur {
			if _, ok := cur[i].Apps[id]; ok {
				delete(cur[i].Apps, id)
				revoked = append(revoked, cur[i].Name)
			}
			if len(cur[i].Apps) == 0 {
				cur[i].Apps = nil
			}
		}
		return cur, nil
	})
	if err != nil {
		if s.log != nil {
			s.log.Error("akses folder aplikasi gagal dicabut", "app", id, "err", err)
		}
		return "system_sync_failed"
	}
	if len(revoked) == 0 {
		return ""
	}
	warning := s.syncAfterChange(r)
	s.record(r, actor, "apps.folders", warning == "", map[string]string{"app": auditText(id), "folders": "", "revoked": auditText(strings.Join(revoked, ","))})
	return warning
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
	// Akses aplikasi ke shared folder ikut dicabut (dipasang lagi harus
	// diberikan ulang).
	warning := s.purgeAppFolders(r, actor, id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
}

// appIDRe: ID aplikasi katalog (sama dengan sapod).
var appIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,22}$`)

// maxAppFolders membatasi jumlah shared folder per aplikasi.
const maxAppFolders = 100

// handleAppFolders mengatur shared folder yang dipasang di container
// aplikasi (keadaan lengkap: folder yang tidak disebut dicabut). Memberi
// program akses ke data: butuh password admin dan tercatat di log audit.
func (s *Server) handleAppFolders(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// Folders: nama shared folder → "read" atau "write".
		Folders       map[string]string `json:"folders"`
		AdminPassword string            `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if !appIDRe.MatchString(id) || len(req.Folders) > maxAppFolders {
		writeError(w, http.StatusNotFound, "not_found", "aplikasi tidak ditemukan")
		return
	}
	// Mencabut semua akses selalu boleh, juga untuk aplikasi yang sudah
	// tidak terpasang atau tidak ada di katalog (sisa akses lama).
	if len(req.Folders) == 0 {
		admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": s.purgeAppFolders(r, admin.Username, id)})
		return
	}
	var catalog []proto.CatalogApp
	if err := s.agent.Call(r.Context(), proto.OpAppsCatalog, nil, &catalog); err != nil {
		s.agentError(w, proto.OpAppsCatalog, err)
		return
	}
	i := slices.IndexFunc(catalog, func(a proto.CatalogApp) bool { return a.ID == id })
	if i < 0 {
		writeError(w, http.StatusNotFound, "not_found", "aplikasi tidak ada di katalog")
		return
	}
	app := catalog[i]
	var installed []proto.InstalledApp
	if err := s.agent.Call(r.Context(), proto.OpAppsList, nil, &installed); err != nil {
		s.agentError(w, proto.OpAppsList, err)
		return
	}
	if !slices.ContainsFunc(installed, func(a proto.InstalledApp) bool { return a.ID == id && a.Installed }) {
		writeError(w, http.StatusNotFound, "not_found", "aplikasi tidak terpasang")
		return
	}
	if app.SharePath == "" {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "aplikasi ini tidak memakai shared folder", map[string]any{"field": "folders"})
		return
	}
	for name, level := range req.Folders {
		_, exists := s.shares.get(name)
		if !exists || (level != "read" && level != "write") || (level == "write" && !app.ShareWrite) {
			writeErrorDetail(w, http.StatusBadRequest, "bad_request", "folder atau izin tidak valid", map[string]any{"field": "folders"})
			return
		}
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	err := s.shares.mutate(func(cur []Share) ([]Share, error) {
		for i := range cur {
			level, give := req.Folders[cur[i].Name]
			if give {
				if cur[i].Apps == nil {
					cur[i].Apps = map[string]string{}
				}
				cur[i].Apps[id] = level
			} else {
				delete(cur[i].Apps, id)
			}
			if len(cur[i].Apps) == 0 {
				cur[i].Apps = nil
			}
		}
		return cur, nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan shared folder")
		return
	}
	warning := s.syncAfterChange(r)
	s.record(r, admin.Username, "apps.folders", warning == "", map[string]string{"app": auditText(id), "folders": permText(req.Folders)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
}
