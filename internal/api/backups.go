package api

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Backup shared folder ke pool lain (ADR 0017). sapod menyimpan tugas dan
// menjalankannya; sapo-api memeriksa izin, mencatat audit, dan mendaftarkan
// folder hasil pemulihan sebagai shared folder.

var backupIDUnsafe = regexp.MustCompile(`[^a-z0-9_-]+`)

// backupID: <folder>-<pool tujuan>, aman untuk nama dataset.
func backupID(share, dest string) string {
	id := backupIDUnsafe.ReplaceAllString(strings.ToLower(share+"-"+dest), "-")
	id = strings.TrimLeft(id, "0123456789_-")
	if len(id) > 41 {
		id = strings.TrimRight(id[:41], "_-")
	}
	return id
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	var out proto.BackupsListResult
	if err := s.agent.Call(r.Context(), proto.OpBackupsList, nil, &out); err != nil {
		s.agentError(w, proto.OpBackupsList, err)
		return
	}
	s.registerRestored(r, out)
	writeJSON(w, http.StatusOK, out)
}

// registerRestored mendaftarkan folder pulihan yang sudah selesai dan masih
// ada di pool (Present), dengan izin folder asalnya bila folder itu masih
// ada. Folder yang kemudian dihapus admin tidak Present lagi, jadi tidak
// pernah didaftarkan ulang (dan tidak pernah dibuat kosong oleh sinkron).
func (s *Server) registerRestored(r *http.Request, out proto.BackupsListResult) {
	jobs := map[string]proto.BackupStatus{}
	for _, j := range out.Jobs {
		jobs[j.ID] = j
	}
	var added []Share
	for _, rs := range out.Restores {
		if !rs.OK || !rs.Present {
			continue
		}
		if _, ok := s.shares.get(rs.Name); ok {
			continue
		}
		sh := Share{Name: rs.Name, Pool: rs.Pool, Permissions: map[string]string{}, CreatedAt: s.now().UTC()}
		if j, ok := jobs[rs.JobID]; ok {
			if src, ok := s.shares.get(j.Share); ok {
				sh.Permissions = cloneMap(src.Permissions)
			}
		}
		err := s.shares.mutate(func(cur []Share) ([]Share, error) {
			if slices.ContainsFunc(cur, func(x Share) bool { return x.Name == sh.Name }) {
				return nil, errShareExists
			}
			return append(cur, sh), nil
		})
		if err == nil {
			added = append(added, sh)
		}
	}
	if len(added) == 0 {
		return
	}
	_, syncErr := s.syncSystem(r.Context())
	for _, sh := range added {
		s.record(r, "sapod", "backups.restored", syncErr == nil, map[string]string{"share": sh.Name, "pool": sh.Pool, "permissions": permText(sh.Permissions)})
	}
}

// handleBackupSet membuat atau mengubah tugas backup. Butuh password admin:
// tugas baru menyalin data ke drive lain (bisa dicabut dan dibawa pergi),
// dan mengurangi jumlah simpan membuang salinan lama.
func (s *Server) handleBackupSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID            string `json:"id"`
		Share         string `json:"share"`
		DestPool      string `json:"dest_pool"`
		Hour          int    `json:"hour"`
		Keep          int    `json:"keep"`
		Enabled       bool   `json:"enabled"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sh, ok := s.shares.get(req.Share)
	if !ok {
		writeErrorDetail(w, http.StatusNotFound, "not_found", "shared folder tidak ditemukan", map[string]any{"field": "share"})
		return
	}
	if !shareNameRe.MatchString(req.DestPool) || req.DestPool == sh.Pool {
		writeErrorDetail(w, http.StatusBadRequest, "backup_same_pool", "pool tujuan harus berbeda dari pool folder", map[string]any{"field": "dest_pool"})
		return
	}
	if req.Hour < 0 || req.Hour > 23 || req.Keep < 1 || req.Keep > 366 {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "jam 0–23 dan jumlah simpan 1–366", map[string]any{"field": "keep"})
		return
	}
	id := req.ID
	if id == "" {
		id = backupID(sh.Name, req.DestPool)
	}
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "ID tugas tidak valid")
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	job := proto.BackupJob{ID: id, SrcPool: sh.Pool, Share: sh.Name, DestPool: req.DestPool, Hour: req.Hour, Keep: req.Keep, Enabled: req.Enabled}
	err := s.agent.Call(r.Context(), proto.OpBackupsSet, job, nil)
	detail := map[string]string{"id": id, "share": sh.Name, "pool": sh.Pool, "dest_pool": auditText(req.DestPool),
		"hour": strconv.Itoa(req.Hour), "keep": strconv.Itoa(req.Keep), "enabled": strconv.FormatBool(req.Enabled)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "backups.set", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpBackupsSet, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleBackupRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.agent.Call(r.Context(), proto.OpBackupsRun, proto.BackupDeleteParams{ID: id}, nil)
	sess, _, _ := s.currentSession(r)
	detail := map[string]string{"id": auditText(id)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, sess.Username, "backups.run", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpBackupsRun, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleBackupDelete menghapus tugas; dengan delete_data juga semua salinan
// di tujuan (ketik ulang ID tugas). Data di folder asal tidak tersentuh.
func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeleteData    bool   `json:"delete_data"`
		ConfirmName   string `json:"confirm_name"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if req.DeleteData && req.ConfirmName != id {
		writeErrorDetail(w, http.StatusBadRequest, "confirm_mismatch", "nama yang diketik ulang tidak sama", map[string]any{"field": "confirm_name"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	s.destructive(w, r, admin, "backups.delete", map[string]string{"id": auditText(id), "delete_data": strconv.FormatBool(req.DeleteData)},
		proto.OpBackupsDelete, proto.BackupDeleteParams{ID: id, DeleteData: req.DeleteData}, nil)
}

// handleBackupRestore memulihkan satu salinan sebagai shared folder BARU
// (folder yang ada tidak pernah ditimpa). Butuh password admin.
func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Copy          string `json:"copy"`
		Name          string `json:"name"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(strings.ToLower(req.Name))
	if !shareNameRe.MatchString(req.Name) || slices.Contains(reservedShareNames, req.Name) {
		writeErrorDetail(w, http.StatusBadRequest, "share_invalid", "nama shared folder tidak sesuai aturan", map[string]any{"field": "name"})
		return
	}
	ext, err := s.externalNames(r)
	if err != nil {
		s.agentError(w, proto.OpExternalList, err)
		return
	}
	if _, taken := s.shares.get(req.Name); taken || ext[req.Name] {
		writeErrorDetail(w, http.StatusConflict, "share_taken", "nama shared folder sudah dipakai", map[string]any{"field": "name"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	id := r.PathValue("id")
	err = s.agent.Call(r.Context(), proto.OpBackupsRestore, proto.BackupRestoreParams{ID: id, Copy: req.Copy, Name: req.Name}, nil)
	detail := map[string]string{"id": auditText(id), "copy": auditText(req.Copy), "share": req.Name}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "backups.restore", err == nil, detail)
	if err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) && pe.Code == proto.ErrConflict {
			writeErrorDetail(w, http.StatusConflict, "share_taken", pe.Message, map[string]any{"field": "name"})
			return
		}
		s.agentError(w, proto.OpBackupsRestore, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
