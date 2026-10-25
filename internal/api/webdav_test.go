package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

// brokenBody: unggahan yang terputus di tengah jalan.
type brokenBody struct{ sent bool }

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errors.New("koneksi terputus")
	}
	b.sent = true
	return copy(p, "sebagian"), nil
}

func TestWebDAV(t *testing.T) {
	agent := shareAgent()
	dir := t.TempDir()
	download, upload := filepath.Join(dir, "unduh"), filepath.Join(dir, "unggah")
	_ = os.WriteFile(download, []byte("isi file"), 0o600)
	_ = os.WriteFile(upload, nil, 0o600)
	agent.files = map[string]string{proto.OpFilesOpenRead: download, proto.OpFilesOpenWrite: upload}
	agent.results[proto.OpFilesList] = proto.FilesListResult{Entries: []proto.FileEntry{{Name: "a.txt", Type: "file", Size: 8}, {Name: "tautan", Type: "symlink"}}}
	agent.results[proto.OpFilesStat] = proto.FileEntry{Name: "keluarga", Type: "dir"}
	agent.results[proto.OpFilesOpenRead] = proto.FilesOpenResult{Size: 8}
	agent.results[proto.OpFilesOpenWrite] = proto.FilesOpenResult{Temp: ".sapo-upload-0123456789abcdef"}
	agent.results[proto.OpFilesCommit] = map[string]bool{"ok": true}
	agent.results[proto.OpFilesAbort] = map[string]bool{"ok": true}
	agent.results[proto.OpFilesMkdir] = map[string]bool{"ok": true}
	h := newHarness(t, agent)
	h.runSetup()
	h.createUser("budi", "user", false)
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "keluarga", "pool": "tank", "admin_password": adminPassword,
		"permissions": map[string]string{"user:budi": "write"}})
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "arsip", "pool": "tank", "admin_password": adminPassword,
		"permissions": map[string]string{"user:budi": "read"}})
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "rahasia", "pool": "tank", "admin_password": adminPassword,
		"permissions": map[string]string{}})
	h.loginAs("budi", userPassword)
	rec := h.do(http.MethodPost, "/api/v1/account/app-passwords", map[string]any{"name": "Laptop", "password": userPassword})
	if rec.Code != http.StatusOK {
		t.Fatalf("buat password aplikasi: %d %s", rec.Code, rec.Body)
	}
	secret := between(rec.Body.String(), `"password":"`, `"`)
	id := between(rec.Body.String(), `"id":"`, `"`)
	if rec := h.do(http.MethodGet, "/api/v1/account/app-passwords", nil); strings.Contains(rec.Body.String(), "hash") || !strings.Contains(rec.Body.String(), "Laptop") {
		t.Fatalf("daftar password aplikasi: %s", rec.Body)
	}

	dav := func(method, path, user, pass string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, body)
		r.RemoteAddr = "192.168.1.20:40000"
		if user != "" {
			r.SetBasicAuth(user, pass)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.s.ServeHTTP(w, r)
		return w
	}
	if w := dav("PROPFIND", "/dav/", "", "", nil); w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("tanpa login: %d %v", w.Code, w.Header())
	}
	// Password akun (bukan password aplikasi) tidak berlaku untuk WebDAV.
	if w := dav("PROPFIND", "/dav/", "budi", userPassword, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("password akun: %d", w.Code)
	}
	w := dav("PROPFIND", "/dav/", "budi", secret, nil, "Depth", "1")
	if w.Code != http.StatusMultiStatus || !strings.Contains(w.Body.String(), "/dav/keluarga/") || !strings.Contains(w.Body.String(), "/dav/arsip/") || strings.Contains(w.Body.String(), "rahasia") {
		t.Fatalf("akar: %d %s", w.Code, w.Body)
	}
	w = dav("PROPFIND", "/dav/keluarga/", "budi", secret, nil, "Depth", "1")
	if w.Code != http.StatusMultiStatus || !strings.Contains(w.Body.String(), "a.txt") || strings.Contains(w.Body.String(), "tautan") {
		t.Fatalf("isi keluarga (symlink tidak tampil): %d %s", w.Code, w.Body)
	}
	if w := dav("PROPFIND", "/dav/rahasia/", "budi", secret, nil, "Depth", "0"); w.Code != http.StatusNotFound {
		t.Fatalf("folder tanpa izin: %d", w.Code)
	}

	// Unggah utuh: diberi nama akhir dan menimpa file lama.
	agent.mu.Lock()
	agent.results[proto.OpFilesStat] = proto.FileEntry{Name: "a.txt", Type: "file", Size: 8}
	agent.mu.Unlock()
	agent.forget(proto.OpFilesCommit)
	if w := dav(http.MethodPut, "/dav/keluarga/foto/b.txt", "budi", secret, strings.NewReader("halo")); w.Code >= 300 {
		t.Fatalf("unggah: %d %s", w.Code, w.Body)
	}
	if p, _ := agent.param(proto.OpFilesCommit).(proto.FilesCommitParams); p.Path != "foto/b.txt" || !p.Overwrite || p.Share != "keluarga" || p.User != "budi" {
		t.Fatalf("commit = %+v", p)
	}
	if p, _ := agent.param(proto.OpFilesOpenWrite).(proto.FilesTarget); p.Path != "foto" {
		t.Fatalf("open_write = %+v", p)
	}
	// Unggahan terputus: file sementara dibuang, file lama tidak tertimpa.
	agent.forget(proto.OpFilesCommit)
	agent.forget(proto.OpFilesAbort)
	dav(http.MethodPut, "/dav/keluarga/b.txt", "budi", secret, &brokenBody{})
	if agent.called(proto.OpFilesCommit) || !agent.called(proto.OpFilesAbort) {
		t.Fatal("unggahan terputus tidak boleh diberi nama akhir")
	}
	// Folder baca saja: tidak bisa ditulis.
	agent.forget(proto.OpFilesOpenWrite)
	if w := dav(http.MethodPut, "/dav/arsip/x.txt", "budi", secret, strings.NewReader("x")); w.Code != http.StatusForbidden || agent.called(proto.OpFilesOpenWrite) {
		t.Fatalf("tulis ke folder baca saja: %d", w.Code)
	}
	// Pindah ke folder baca saja dan menghapus akar folder: ditolak.
	if w := dav("MOVE", "/dav/keluarga/a.txt", "budi", secret, nil, "Destination", "http://example.com/dav/arsip/a.txt"); w.Code != http.StatusForbidden {
		t.Fatalf("pindah ke folder baca saja: %d", w.Code)
	}
	if w := dav(http.MethodDelete, "/dav/keluarga", "budi", secret, nil); w.Code != http.StatusForbidden {
		t.Fatalf("hapus akar folder: %d", w.Code)
	}
	if w := dav("MKCOL", "/dav/keluarga/baru", "budi", secret, nil); w.Code != http.StatusCreated {
		t.Fatalf("buat folder: %d %s", w.Code, w.Body)
	}
	// Unduh: selalu sebagai lampiran dengan CSP sandbox.
	w = dav(http.MethodGet, "/dav/keluarga/a.txt", "budi", secret, nil)
	if w.Code != http.StatusOK || w.Body.String() != "isi file" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("unduh: %d %q %v", w.Code, w.Body, w.Header())
	}
	// Dari halaman web lain: ditolak.
	if w := dav(http.MethodDelete, "/dav/keluarga/a.txt", "budi", secret, nil, "Origin", "https://jahat.example"); w.Code != http.StatusForbidden {
		t.Fatalf("Origin lain: %d", w.Code)
	}
	// Dicabut: tidak berlaku lagi.
	if rec := h.do(http.MethodPost, "/api/v1/account/app-passwords/"+id+"/delete", nil); rec.Code != http.StatusOK {
		t.Fatalf("cabut: %d %s", rec.Code, rec.Body)
	}
	if w := dav("PROPFIND", "/dav/", "budi", secret, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("password yang dicabut: %d", w.Code)
	}
}

// WebDAV membawa password di setiap permintaan: tidak pernah lewat HTTP
// biasa saat HTTPS wajib.
func TestWebDAVNotOverPlainHTTP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/dav/keluarga/a.txt", nil)
	if plainHTTPAllowed(r) {
		t.Fatal("WebDAV tidak boleh lewat HTTP")
	}
}

func between(s, a, b string) string {
	_, rest, _ := strings.Cut(s, a)
	v, _, _ := strings.Cut(rest, b)
	return v
}
