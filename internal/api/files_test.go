package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestFileStation(t *testing.T) {
	agent := shareAgent()
	dir := t.TempDir()
	download := filepath.Join(dir, "unduh")
	upload := filepath.Join(dir, "unggah")
	_ = os.WriteFile(download, []byte("isi foto"), 0o600)
	_ = os.WriteFile(upload, nil, 0o600)
	agent.files = map[string]string{proto.OpFilesOpenRead: download, proto.OpFilesOpenWrite: upload}
	agent.results[proto.OpFilesList] = proto.FilesListResult{Entries: []proto.FileEntry{{Name: "a.jpg", Type: "file", Size: 8}}}
	agent.results[proto.OpFilesOpenRead] = proto.FilesOpenResult{Size: 8}
	agent.results[proto.OpFilesOpenWrite] = proto.FilesOpenResult{Temp: ".sapo-upload-0123456789abcdef"}
	agent.results[proto.OpFilesCommit] = map[string]bool{"ok": true}
	agent.results[proto.OpFilesAbort] = map[string]bool{"ok": true}
	agent.results[proto.OpFilesDelete] = map[string]bool{"ok": true}
	h := newHarness(t, agent)
	h.runSetup()
	h.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": "ortu"})
	h.createUser("budi", "user", false, "ortu")
	h.createUser("ani", "user", false)
	h.createUser("cici", "user", false)
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "keluarga", "pool": "tank", "admin_password": adminPassword,
		"permissions": map[string]string{"group:ortu": "write", "user:cici": "read"}})
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "kantor", "pool": "tank", "admin_password": adminPassword,
		"permissions": map[string]string{"user:ani": "write"}})

	// budi (lewat grup ortu): hanya keluarga, baca & tulis.
	h.loginAs("budi", userPassword)
	if rec := h.do(http.MethodGet, "/api/v1/files/shares", nil); rec.Code != http.StatusOK || rec.Body.String() != `[{"name":"keluarga","access":"write"}]`+"\n" {
		t.Fatalf("folder budi: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/files/list?share=keluarga&path=/foto/", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "a.jpg") {
		t.Fatalf("daftar: %d %s", rec.Code, rec.Body)
	}
	// Selalu atas nama pengguna sesi, bukan dari permintaan.
	if p, _ := agent.param(proto.OpFilesList).(proto.FilesTarget); p != (proto.FilesTarget{User: "budi", Pool: "tank", Share: "keluarga", Path: "foto"}) {
		t.Fatalf("files.list = %+v", p)
	}
	if rec := h.do(http.MethodGet, "/api/v1/files/list?share=kantor", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("folder tanpa izin: %d", rec.Code)
	}

	rec := h.do(http.MethodGet, "/api/v1/files/download?share=keluarga&path=foto/a.jpg", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "isi foto" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename=a.jpg` ||
		rec.Header().Get("Content-Type") != "application/octet-stream" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("unduh: %d %q %v", rec.Code, rec.Body, rec.Header())
	}

	body := func(s string) reqOpt { return func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(s)) } }
	if rec := h.do(http.MethodPost, "/api/v1/files/upload?share=keluarga&path=foto&name=b.txt", nil, body("isi baru")); rec.Code != http.StatusOK {
		t.Fatalf("unggah: %d %s", rec.Code, rec.Body)
	}
	if b, _ := os.ReadFile(upload); string(b) != "isi baru" {
		t.Fatalf("isi unggahan = %q", b)
	}
	if p, _ := agent.param(proto.OpFilesCommit).(proto.FilesCommitParams); p.Path != "foto/b.txt" || p.Temp != ".sapo-upload-0123456789abcdef" || p.Overwrite || p.User != "budi" {
		t.Fatalf("files.commit = %+v", p)
	}
	if rec := h.do(http.MethodPost, "/api/v1/files/upload?share=keluarga&name=../x", nil, body("x")); rec.Code != http.StatusBadRequest {
		t.Fatalf("nama dengan /: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/files/delete", map[string]string{"share": "keluarga", "path": "foto/b.txt"}); rec.Code != http.StatusOK {
		t.Fatalf("hapus: %d %s", rec.Code, rec.Body)
	}

	// cici: baca saja, perubahan ditolak sebelum sampai sapod.
	h.loginAs("cici", userPassword)
	agent.forget(proto.OpFilesDelete)
	if rec := h.do(http.MethodPost, "/api/v1/files/delete", map[string]string{"share": "keluarga", "path": "foto"}); rec.Code != http.StatusForbidden || agent.called(proto.OpFilesDelete) {
		t.Fatalf("hapus oleh pembaca: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/files/upload?share=keluarga&name=x", nil, body("x")); rec.Code != http.StatusForbidden {
		t.Fatalf("unggah oleh pembaca: %d", rec.Code)
	}
	// Tanpa login: ditolak.
	h.cookie = nil
	if rec := h.do(http.MethodGet, "/api/v1/files/shares", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa login: %d", rec.Code)
	}
}
