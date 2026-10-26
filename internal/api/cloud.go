package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Backup ke S3/cloud (ADR 0020). sapod menyimpan kredensial dan menjalankan
// restic; sapo-api memeriksa izin, meminta password admin, dan mencatat
// audit (tanpa rahasia).

func (s *Server) handleCloud(w http.ResponseWriter, r *http.Request) {
	var out proto.CloudListResult
	if err := s.agent.Call(r.Context(), proto.OpCloudList, nil, &out); err != nil {
		s.agentError(w, proto.OpCloudList, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCloudRepoAdd: repo baru (password acak, tampil sekali) atau
// menyambung repo yang ada (password dari admin).
func (s *Server) handleCloudRepoAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		proto.CloudRepoParams
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	p := req.CloudRepoParams
	p.Name, p.Endpoint, p.Bucket = strings.TrimSpace(p.Name), strings.TrimSpace(p.Endpoint), strings.TrimSpace(p.Bucket)
	p.Prefix, p.Region = strings.Trim(strings.TrimSpace(p.Prefix), "/"), strings.TrimSpace(p.Region)
	p.AccessKey, p.SecretKey = strings.TrimSpace(p.AccessKey), strings.TrimSpace(p.SecretKey)
	p.Endpoint = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(p.Endpoint, "https://"), "http://"), "/")
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(160 * time.Second))
	var out proto.CloudRepoAdded
	err := s.agent.Call(ctx, proto.OpCloudRepoAdd, p, &out)
	detail := map[string]string{"name": auditText(p.Name), "endpoint": auditText(p.Endpoint), "bucket": auditText(p.Bucket),
		"prefix": auditText(p.Prefix), "existing": strconv.FormatBool(p.Existing), "insecure_http": strconv.FormatBool(p.InsecureHTTP)}
	if err != nil {
		detail["error"] = errorCode(err)
	} else {
		detail["repo"] = out.ID
	}
	s.record(r, admin.Username, "cloud.repo_add", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpCloudRepoAdd, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCloudRepoDelete: NAS ini melupakan repo (data di cloud tetap).
func (s *Server) handleCloudRepoDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	id := r.PathValue("id")
	err := s.agent.Call(r.Context(), proto.OpCloudRepoDelete, proto.CloudIDParams{ID: id}, nil)
	detail := map[string]string{"repo": auditText(id)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "cloud.repo_delete", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpCloudRepoDelete, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCloudSnapshots(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5*time.Minute + 10*time.Second))
	var out []proto.CloudSnapshot
	if err := s.agent.Call(ctx, proto.OpCloudSnapshots, proto.CloudIDParams{ID: r.PathValue("id")}, &out); err != nil {
		s.agentError(w, proto.OpCloudSnapshots, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCloudLs: isi satu folder di dalam salinan (nama file saja), untuk
// memilih file/folder yang dipulihkan.
func (s *Server) handleCloudLs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5*time.Minute + 10*time.Second))
	var out proto.CloudLsResult
	p := proto.CloudLsParams{Repo: r.PathValue("id"), Snapshot: r.PathValue("snap"), Path: r.URL.Query().Get("path")}
	if err := s.agent.Call(ctx, proto.OpCloudLs, p, &out); err != nil {
		s.agentError(w, proto.OpCloudLs, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCloudJobSet: butuh password admin (data keluar dari NAS, dan lama
// simpan yang lebih pendek membuang salinan lama).
func (s *Server) handleCloudJobSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID            string `json:"id"`
		Repo          string `json:"repo"`
		Share         string `json:"share"`
		Hour          int    `json:"hour"`
		KeepDays      int    `json:"keep_days"`
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
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	job := proto.CloudJob{ID: req.ID, Repo: req.Repo, Pool: sh.Pool, Share: sh.Name, Hour: req.Hour, KeepDays: req.KeepDays, Enabled: req.Enabled}
	var out map[string]string
	err := s.agent.Call(r.Context(), proto.OpCloudJobSet, job, &out)
	detail := map[string]string{"id": auditText(out["id"]), "repo": auditText(req.Repo), "share": sh.Name, "hour": strconv.Itoa(req.Hour),
		"keep_days": strconv.Itoa(req.KeepDays), "enabled": strconv.FormatBool(req.Enabled)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "cloud.job_set", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpCloudJobSet, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCloudJobRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.agent.Call(r.Context(), proto.OpCloudRun, proto.CloudIDParams{ID: id}, nil)
	sess, _, _ := s.currentSession(r)
	detail := map[string]string{"id": auditText(id)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, sess.Username, "cloud.run", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpCloudRun, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCloudJobDelete: tugas dihapus; salinan di cloud tetap.
func (s *Server) handleCloudJobDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	id := r.PathValue("id")
	err := s.agent.Call(r.Context(), proto.OpCloudJobDelete, proto.CloudIDParams{ID: id}, nil)
	detail := map[string]string{"id": auditText(id)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "cloud.job_delete", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpCloudJobDelete, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCloudRestore: salinan (atau satu file/folder di dalamnya, Path)
// dipulihkan menjadi shared folder baru (tanpa izin pengguna bila folder asal
// tidak ada di NAS ini). Didaftarkan lewat
// GET /api/v1/backups setelah selesai, seperti pemulihan lain.
func (s *Server) handleCloudRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Snapshot      string `json:"snapshot"`
		Pool          string `json:"pool"`
		Name          string `json:"name"`
		Path          string `json:"path"`
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
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5*time.Minute + 10*time.Second))
	repo := r.PathValue("id")
	err = s.agent.Call(ctx, proto.OpCloudRestore, proto.CloudRestoreParams{Repo: repo, Snapshot: req.Snapshot, Pool: req.Pool, Name: req.Name, Path: req.Path}, nil)
	detail := map[string]string{"repo": auditText(repo), "snapshot": auditText(req.Snapshot), "pool": auditText(req.Pool), "share": req.Name}
	if req.Path != "" {
		detail["path"] = auditText(req.Path)
	}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "cloud.restore", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpCloudRestore, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
