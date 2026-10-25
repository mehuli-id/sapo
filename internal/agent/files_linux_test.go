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

	// stat: file, direktori, dan akar; symlink tidak diikuti.
	if res, err := do(fsRequest{Op: "stat", Path: "foto/a.jpg"}); err != nil || res.(proto.FileEntry).Type != "file" || res.(proto.FileEntry).Size != 6 || res.(proto.FileEntry).Name != "a.jpg" {
		t.Fatalf("stat file: %+v %v", res, err)
	}
	if res, err := do(fsRequest{Op: "stat", Path: ""}); err != nil || res.(proto.FileEntry).Type != "dir" {
		t.Fatalf("stat akar: %+v %v", res, err)
	}
	if _, err := do(fsRequest{Op: "stat", Path: "keluar/rahasia"}); err == nil {
		t.Fatal("stat lewat symlink harus ditolak")
	}
	if _, err := do(fsRequest{Op: "stat", Path: "hantu"}); code(err) != proto.ErrNotFound {
		t.Fatalf("stat yang tidak ada: %v", err)
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
	// Overwrite: file biasa ditimpa atomik; direktori tidak pernah ditimpa.
	if _, err := do(fsRequest{Op: "rename", Path: "c.txt", To: "dokumen/b.jpg", Overwrite: true}); err != nil {
		t.Fatalf("rename menimpa file: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "dokumen/b.jpg")); string(b) != "c" {
		t.Fatalf("isi setelah menimpa = %q", b)
	}
	_ = os.WriteFile(filepath.Join(root, "d.txt"), []byte("d"), 0o644)
	if _, err := do(fsRequest{Op: "rename", Path: "d.txt", To: "foto", Overwrite: true}); code(err) != errFileExists {
		t.Fatalf("rename tidak boleh menimpa direktori: %v", err)
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

	// Menimpa file yang hanya boleh dibaca ditolak walau direktorinya
	// boleh ditulis.
	_ = os.WriteFile(filepath.Join(root, "baca.txt"), []byte("lama"), 0o444)
	res, _ = do(fsRequest{Op: "open_write", Path: ""})
	f, _ = recvFile(pr)
	f.Close()
	if _, err := do(fsRequest{Op: "commit", Path: "baca.txt", Temp: res.(proto.FilesOpenResult).Temp, Overwrite: true}); code(err) != errFilePermission {
		t.Fatalf("menimpa file baca-saja: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "baca.txt")); string(b) != "lama" {
		t.Fatal("file baca-saja tertimpa")
	}
	// Nama yang bukan UTF-8 tampil sebagai "other" (tidak bisa dipakai).
	_ = os.WriteFile(filepath.Join(root, "lama-\xe9.txt"), nil, 0o644)
	if res, _ := do(fsRequest{Op: "list", Path: "", Max: 100}); !strings.Contains(mustJSON(res), `"type":"other"`) {
		t.Fatalf("nama bukan UTF-8: %s", mustJSON(res))
	}

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

func TestFSHelperRestore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("berjalan sebagai root")
	}
	base := t.TempDir()
	live, snap := filepath.Join(base, "live"), filepath.Join(base, "snap")
	for _, d := range []string{filepath.Join(live, "dok"), filepath.Join(snap, "dok/arsip")} {
		_ = os.MkdirAll(d, 0o755)
	}
	_ = os.WriteFile(filepath.Join(snap, "dok/laporan.txt"), []byte("versi lama"), 0o644)
	_ = os.WriteFile(filepath.Join(snap, "dok/arsip/a.txt"), []byte("a"), 0o644)
	_ = os.Symlink("/etc/passwd", filepath.Join(snap, "dok/arsip/tautan"))
	do := func(path string, overwrite bool, maxFiles int) (proto.FilesRestoreResult, error) {
		res, err := fsDo(fsRequest{Op: "restore", Root: live, Src: snap, Path: path, Overwrite: overwrite, Label: "dipulihkan 2026-10-28 02.00",
			Replaced: "sebelum dipulihkan 2026-10-28 09.00", MaxFiles: maxFiles, MaxBytes: 1 << 20})
		if err != nil {
			return proto.FilesRestoreResult{}, err
		}
		return res.(proto.FilesRestoreResult), nil
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(live, p)); return string(b) }

	// Belum ada: nama asli.
	if r, err := do("dok/laporan.txt", false, 100); err != nil || r.Name != "laporan.txt" || read("dok/laporan.txt") != "versi lama" {
		t.Fatalf("pulihkan: %+v %v", r, err)
	}
	// Sudah ada: nama baru, file sekarang tidak berubah.
	_ = os.WriteFile(filepath.Join(live, "dok/laporan.txt"), []byte("versi baru"), 0o644)
	r, err := do("dok/laporan.txt", false, 100)
	if err != nil || r.Name != "laporan (dipulihkan 2026-10-28 02.00).txt" || read("dok/laporan.txt") != "versi baru" || read("dok/"+r.Name) != "versi lama" {
		t.Fatalf("pulihkan dengan nama baru: %+v %v", r, err)
	}
	if r, _ := do("dok/laporan.txt", false, 100); r.Name != "laporan (dipulihkan 2026-10-28 02.00 2).txt" {
		t.Fatalf("nama kedua = %q", r.Name)
	}
	// Ditimpa bila dipilih, tetapi file sekarang disimpan sebagai cadangan.
	if r, err := do("dok/laporan.txt", true, 100); err != nil || r.Name != "laporan.txt" || read("dok/laporan.txt") != "versi lama" ||
		read("dok/laporan (sebelum dipulihkan 2026-10-28 09.00).txt") != "versi baru" {
		t.Fatalf("timpa: %+v %v", r, err)
	}
	// Ruang kosong tidak cukup: ditolak sebelum menyalin.
	if _, err := fsDo(fsRequest{Op: "restore", Root: live, Src: snap, Path: "dok/laporan.txt", Label: "x {time}", MaxFiles: 100, MaxBytes: 1 << 20, Avail: 1}); fsErrorCode(err) != "restore_no_space" {
		t.Fatalf("ruang kosong: %v", err)
	}
	// Folder: disalin utuh, symlink dilewati.
	r, err = do("dok/arsip", false, 100)
	if err != nil || r.Name != "arsip" || read("dok/arsip/a.txt") != "a" || r.Skipped != 1 {
		t.Fatalf("pulihkan folder: %+v %v", r, err)
	}
	if _, err := os.Lstat(filepath.Join(live, "dok/arsip/tautan")); err == nil {
		t.Fatal("symlink tidak boleh dipulihkan")
	}
	// Folder yang sudah ada tidak pernah ditimpa, walau Overwrite.
	if r, err := do("dok/arsip", true, 100); err != nil || r.Name != "arsip (dipulihkan 2026-10-28 02.00)" {
		t.Fatalf("folder yang ada: %+v %v", r, err)
	}
	// Melebihi batas: gagal, tanpa sisa salinan setengah jadi.
	_ = os.RemoveAll(filepath.Join(live, "dok"))
	_ = os.MkdirAll(filepath.Join(live, "dok"), 0o755)
	if _, err := do("dok/arsip", false, 1); fsErrorCode(err) != "restore_too_large" {
		t.Fatalf("batas: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(live, "dok")); len(entries) != 0 {
		t.Fatalf("sisa salinan: %v", entries)
	}
	// Path lewat symlink atau keluar dari akar: ditolak.
	for _, p := range []string{"dok/arsip/tautan", "../live/dok"} {
		if _, err := do(p, false, 100); err == nil {
			t.Errorf("%s harus ditolak", p)
		}
	}
}
