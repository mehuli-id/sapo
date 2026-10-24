package agent

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Test proses pembantu langsung (sebagai pengguna yang menjalankan test).
func TestFSHelperOps(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("berjalan sebagai root")
	}
	root := filepath.Join(t.TempDir(), "keluarga")
	outside := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "foto/2026"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "foto/a.jpg"), []byte("gambar"), 0o644)
	_ = os.WriteFile(filepath.Join(outside, "rahasia"), []byte("jangan"), 0o644)
	_ = os.Symlink(outside, filepath.Join(root, "keluar"))
	_ = os.Symlink(filepath.Join(outside, "rahasia"), filepath.Join(root, "tautan"))
	do := func(req fsRequest) (any, error) {
		req.Root = root
		return fsDo(req)
	}
	code := func(err error) string {
		if err == nil {
			return ""
		}
		return fsErrorCode(err)
	}

	res, err := do(fsRequest{Op: "list", Path: "", Max: 100})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	for _, w := range []string{`"name":"foto","type":"dir"`, `"name":"keluar","type":"symlink"`} {
		if !strings.Contains(string(b), w) {
			t.Errorf("daftar tanpa %s: %s", w, b)
		}
	}
	// Symlink tidak pernah diikuti, keluar dari akar ditolak.
	for _, p := range []string{"keluar", "keluar/rahasia", "tautan"} {
		if _, err := do(fsRequest{Op: "list", Path: p, Max: 10}); err == nil {
			t.Errorf("list %s harus ditolak", p)
		}
	}
	if _, err := do(fsRequest{Op: "open_read", Path: "tautan"}); err == nil {
		t.Fatal("membuka symlink harus ditolak")
	}
	if _, err := do(fsRequest{Op: "list", Path: "../" + filepath.Base(outside), Max: 10}); err == nil {
		t.Fatal(".. harus ditolak")
	}

	if _, err := do(fsRequest{Op: "mkdir", Path: "dokumen"}); err != nil {
		t.Fatal(err)
	}
	if _, err := do(fsRequest{Op: "mkdir", Path: "dokumen"}); code(err) != errFileExists {
		t.Fatalf("mkdir ganda: %v", err)
	}
	if _, err := do(fsRequest{Op: "rename", Path: "foto/a.jpg", To: "dokumen/b.jpg"}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "c.txt"), []byte("c"), 0o644)
	if _, err := do(fsRequest{Op: "rename", Path: "c.txt", To: "dokumen/b.jpg"}); code(err) != errFileExists {
		t.Fatalf("rename tidak boleh menimpa: %v", err)
	}

	// Unggah: file sementara lewat fd, lalu diberi nama.
	pr, pw, err := socketPair()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	fdSocket = int(pw.Fd())
	defer func() { fdSocket = 3 }()
	res, err = do(fsRequest{Op: "open_write", Path: "dokumen"})
	if err != nil {
		t.Fatal(err)
	}
	temp := res.(proto.FilesOpenResult).Temp
	f, err := recvFile(pr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("isi baru")
	f.Close()
	if res, _ := do(fsRequest{Op: "list", Path: "dokumen", Max: 10}); strings.Contains(mustJSON(res), temp) {
		t.Fatal("file sementara tidak boleh tampil")
	}
	if _, err := do(fsRequest{Op: "commit", Path: "dokumen/b.jpg", Temp: temp}); code(err) != errFileExists {
		t.Fatalf("commit tanpa overwrite tidak boleh menimpa: %v", err)
	}
	if _, err := do(fsRequest{Op: "commit", Path: "dokumen/b.jpg", Temp: temp, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "dokumen/b.jpg")); string(b) != "isi baru" {
		t.Fatalf("isi = %q", b)
	}
	// Overwrite tidak pernah menimpa folder.
	res, _ = do(fsRequest{Op: "open_write", Path: ""})
	f, _ = recvFile(pr)
	f.Close()
	if _, err := do(fsRequest{Op: "commit", Path: "dokumen", Temp: res.(proto.FilesOpenResult).Temp, Overwrite: true}); code(err) != errFileExists {
		t.Fatalf("menimpa folder: %v", err)
	}
	_, _ = do(fsRequest{Op: "abort", Path: "x", Temp: res.(proto.FilesOpenResult).Temp})

	// Unduh.
	res, err = do(fsRequest{Op: "open_read", Path: "dokumen/b.jpg"})
	if err != nil || res.(proto.FilesOpenResult).Size != 8 {
		t.Fatalf("open_read: %+v %v", res, err)
	}
	f, _ = recvFile(pr)
	if b, _ := io.ReadAll(f); string(b) != "isi baru" {
		t.Fatalf("unduh = %q", b)
	}
	f.Close()
	if _, err := do(fsRequest{Op: "open_read", Path: "dokumen"}); err == nil {
		t.Fatal("folder bukan file biasa")
	}

	// Hapus folder beserta isinya; symlink ikut terhapus, targetnya tidak.
	_ = os.Symlink(outside, filepath.Join(root, "foto/2026/keluar"))
	if _, err := do(fsRequest{Op: "delete", Path: "foto"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "foto")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("folder harus terhapus")
	}
	if _, err := os.Stat(filepath.Join(outside, "rahasia")); err != nil {
		t.Fatal("file di luar folder tidak boleh terhapus")
	}
	if _, err := do(fsRequest{Op: "delete", Path: "tidak-ada"}); code(err) != proto.ErrNotFound {
		t.Fatalf("hapus yang tidak ada: %v", err)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
