package api

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mehuli-id/sapo/internal/agent"
	"github.com/mehuli-id/sapo/internal/proto"
)

// --- File Station (ADR 0010) -------------------------------------------------
//
// Semua operasi diteruskan ke sapod atas nama pengguna yang login; sapod
// menjalankannya sebagai akun Linux pengguna itu, jadi izin folder
// ditegakkan kernel. Di sini hanya dipilih folder yang ditampilkan dan
// ditolak lebih awal yang jelas tidak diizinkan.

// fileShare adalah folder yang bisa dibuka pengguna di File Station.
type fileShare struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Access: "read" atau "write".
	Access string `json:"access"`
	// Home: folder pribadi pengguna (Sapo Drive).
	Home bool `json:"home,omitempty"`
}

// userShares: folder yang boleh dibuka pengguna sesi ini, dengan izin
// efektifnya (aturan sama dengan ACL di sistem berkas).
func (s *Server) userShares(r *http.Request) (string, map[string]fileShare, map[string]Share) {
	sess, _, _ := s.currentSession(r)
	u, ok := s.store.Get(sess.Username)
	out, all := map[string]fileShare{}, map[string]Share{}
	if !ok || u.Disabled {
		return sess.Username, out, all
	}
	for _, sh := range s.shares.list() {
		exists := map[string]bool{}
		for p := range sh.Permissions {
			exists[p] = true
		}
		if a := nfsAccess(sh, u, exists); a != "" {
			out[sh.Name] = fileShare{Name: sh.Name, Description: sh.Description, Access: a}
			all[sh.Name] = sh
		}
	}
	return u.Username, out, all
}

func (s *Server) handleFileShares(w http.ResponseWriter, r *http.Request) {
	_, shares, _ := s.userShares(r)
	out := []fileShare{}
	if _, ok := s.homeTarget(r); ok {
		out = append(out, fileShare{Name: homeShare, Access: "write", Home: true})
	}
	for _, sh := range shares {
		out = append(out, sh)
	}
	slices.SortFunc(out, func(a, b fileShare) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, out)
}

// fileTarget menyusun target sapod untuk share dan path dari permintaan.
// write: folder harus berizin tulis.
func (s *Server) fileTarget(w http.ResponseWriter, r *http.Request, share, p string, write bool) (proto.FilesTarget, bool) {
	if share == homeShare {
		t, ok := s.homeTarget(r)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "folder pribadi tidak tersedia")
			return proto.FilesTarget{}, false
		}
		t.Path = strings.Trim(p, "/")
		return t, true
	}
	user, shares, all := s.userShares(r)
	fs, ok := shares[share]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "shared folder tidak ditemukan")
		return proto.FilesTarget{}, false
	}
	if write && fs.Access != "write" {
		writeError(w, http.StatusForbidden, "permission_denied", "folder ini hanya bisa dibaca")
		return proto.FilesTarget{}, false
	}
	return proto.FilesTarget{User: user, Pool: all[share].Pool, Share: share, Path: strings.Trim(p, "/")}, true
}

// homeTarget: folder pribadi pengguna sesi ini, bila Sapo Drive menyala dan
// akunnya aktif.
func (s *Server) homeTarget(r *http.Request) (proto.FilesTarget, bool) {
	pool := s.settings.get().HomesPool
	sess, _, _ := s.currentSession(r)
	u, ok := s.store.Get(sess.Username)
	if pool == "" || !ok || u.Disabled {
		return proto.FilesTarget{}, false
	}
	return proto.FilesTarget{User: u.Username, Pool: pool, Home: true}, true
}

func (s *Server) fileError(w http.ResponseWriter, op string, err error) {
	var pe *proto.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case "permission_denied":
			writeError(w, http.StatusForbidden, pe.Code, pe.Message)
			return
		case "file_exists", "not_empty":
			writeError(w, http.StatusConflict, pe.Code, pe.Message)
			return
		case "timeout":
			writeError(w, http.StatusGatewayTimeout, pe.Code, pe.Message)
			return
		}
	}
	s.agentError(w, op, err)
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	t, ok := s.fileTarget(w, r, q.Get("share"), q.Get("path"), false)
	if !ok {
		return
	}
	var out proto.FilesListResult
	if err := s.agent.Call(r.Context(), proto.OpFilesList, t, &out); err != nil {
		s.fileError(w, proto.OpFilesList, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// keepAlive memperpanjang batas waktu baca/tulis koneksi HTTP selama data
// masih mengalir (unduhan dan unggahan besar melebihi batas server).
// Baca dan tulis diperpanjang bersama: HTTP/2 juga memutus aliran bila
// batas tulis habis selama badan permintaan masih dibaca.
type keepAlive struct {
	rc   *http.ResponseController
	last time.Time
}

func (k *keepAlive) touch() {
	if now := time.Now(); now.Sub(k.last) > 5*time.Second {
		k.last = now
		_ = k.rc.SetReadDeadline(now.Add(60 * time.Second))
		_ = k.rc.SetWriteDeadline(now.Add(60 * time.Second))
	}
}

type deadlineWriter struct {
	w io.Writer
	k *keepAlive
}

func (d deadlineWriter) Write(p []byte) (int, error) {
	d.k.touch()
	return d.w.Write(p)
}

type deadlineReader struct {
	r io.Reader
	k *keepAlive
}

func (d deadlineReader) Read(p []byte) (int, error) {
	d.k.touch()
	return d.r.Read(p)
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	t, ok := s.fileTarget(w, r, q.Get("share"), q.Get("path"), false)
	if !ok {
		return
	}
	var info proto.FilesOpenResult
	f, err := s.agent.CallFile(r.Context(), proto.OpFilesOpenRead, t, &info)
	if err != nil {
		s.fileError(w, proto.OpFilesOpenRead, err)
		return
	}
	defer f.Close()
	// Selalu sebagai lampiran, tidak pernah ditampilkan di halaman panel
	// (file HTML atau SVG dari pengguna tidak boleh berjalan di origin panel).
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(t.Path)}))
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
	k := &keepAlive{rc: http.NewResponseController(w)}
	_, _ = io.Copy(deadlineWriter{w: w, k: k}, io.LimitReader(f, info.Size))
}

func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" || strings.Contains(name, "/") {
		writeError(w, http.StatusBadRequest, "bad_request", "nama file tidak valid")
		return
	}
	dir := strings.Trim(q.Get("path"), "/")
	// Nama diperiksa sebelum isi file dikirim, bukan setelahnya.
	if _, err := agent.CleanFilePath(strings.TrimPrefix(dir+"/"+name, "/")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_params", err.Error())
		return
	}
	t, ok := s.fileTarget(w, r, q.Get("share"), dir, true)
	if !ok {
		return
	}
	var info proto.FilesOpenResult
	f, err := s.agent.CallFile(r.Context(), proto.OpFilesOpenWrite, t, &info)
	if err != nil {
		s.fileError(w, proto.OpFilesOpenWrite, err)
		return
	}
	final := t
	final.Path = strings.TrimPrefix(dir+"/"+name, "/")
	commit := proto.FilesCommitParams{FilesTarget: final, Temp: info.Temp, Overwrite: q.Get("overwrite") == "1"}
	k := &keepAlive{rc: http.NewResponseController(w)}
	k.touch()
	n, copyErr := io.Copy(f, deadlineReader{r: r.Body, k: k})
	closeErr := f.Close()
	ctx := context.WithoutCancel(r.Context())
	if copyErr != nil || closeErr != nil {
		_ = s.agent.Call(ctx, proto.OpFilesAbort, commit, nil)
		s.record(r, t.User, "files.upload", false, map[string]string{"share": t.Share, "path": auditText(final.Path), "bytes": strconv.FormatInt(n, 10), "error": "upload_failed"})
		writeError(w, http.StatusBadRequest, "upload_failed", "unggahan terputus")
		return
	}
	err = s.agent.Call(ctx, proto.OpFilesCommit, commit, nil)
	s.record(r, t.User, "files.upload", err == nil, map[string]string{"share": t.Share, "path": auditText(final.Path), "bytes": strconv.FormatInt(n, 10)})
	if err != nil {
		_ = s.agent.Call(ctx, proto.OpFilesAbort, commit, nil)
		s.fileError(w, proto.OpFilesCommit, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bytes": n})
}

// handleFileChange: mkdir, rename, delete.
func (s *Server) handleFileChange(op, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Share string `json:"share"`
			Path  string `json:"path"`
			To    string `json:"to"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		t, ok := s.fileTarget(w, r, req.Share, req.Path, true)
		if !ok {
			return
		}
		var params any = t
		detail := map[string]string{"share": t.Share, "path": auditText(t.Path)}
		if op == proto.OpFilesRename {
			params = proto.FilesRenameParams{FilesTarget: t, To: strings.Trim(req.To, "/")}
			detail["to"] = auditText(req.To)
		}
		ctx := r.Context()
		if op == proto.OpFilesDelete {
			// Menghapus folder besar bisa lama: tunggu selama sapod mencoba,
			// agar hasil yang dicatat dan dilaporkan sesuai kenyataan.
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), time.Duration(s.cfg.Agent.Files.DeleteTimeoutSeconds+20)*time.Second)
			defer cancel()
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(s.cfg.Agent.Files.DeleteTimeoutSeconds+40) * time.Second))
		}
		err := s.agent.Call(ctx, op, params, nil)
		if err != nil {
			detail["error"] = errorCode(err)
		}
		s.record(r, t.User, action, err == nil, detail)
		if err != nil {
			s.fileError(w, op, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
