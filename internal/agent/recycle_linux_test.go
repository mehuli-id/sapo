package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFSHelperRecycle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("berjalan sebagai root")
	}
	root := filepath.Join(t.TempDir(), "keluarga")
	_ = os.MkdirAll(filepath.Join(root, "foto/2026"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "foto/2026/a.jpg"), []byte("satu"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "foto/b.txt"), []byte("b"), 0o644)
	del := func(path string) error {
		_, err := fsDo(fsRequest{Op: "delete", Root: root, Path: path, Recycle: recycleDir, Label: "dihapus 2026-10-26 10.00"})
		return err
	}
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return "<" + err.Error() + ">"
		}
		return string(b)
	}
	// Letak di tempat sampah sama dengan letak asal; folder perantara dibuat.
	if err := del("foto/2026/a.jpg"); err != nil {
		t.Fatal(err)
	}
	if read("#recycle/foto/2026/a.jpg") != "satu" {
		t.Fatalf("di tempat sampah: %s", read("#recycle/foto/2026/a.jpg"))
	}
	if _, err := os.Stat(filepath.Join(root, "foto/2026/a.jpg")); !os.IsNotExist(err) {
		t.Fatal("file asal harus pindah")
	}
	// Nama yang sama dihapus lagi: tidak menimpa, diberi akhiran.
	_ = os.WriteFile(filepath.Join(root, "foto/2026/a.jpg"), []byte("dua"), 0o644)
	if err := del("foto/2026/a.jpg"); err != nil {
		t.Fatal(err)
	}
	if read("#recycle/foto/2026/a.jpg") != "satu" || read("#recycle/foto/2026/a (dihapus 2026-10-26 10.00).jpg") != "dua" {
		t.Fatal("versi kedua harus disimpan dengan akhiran")
	}
	// Folder beserta isinya.
	if err := del("foto"); err != nil {
		t.Fatal(err)
	}
	if read("#recycle/foto (dihapus 2026-10-26 10.00)/b.txt") != "b" {
		t.Fatalf("folder di tempat sampah: %s", read("#recycle/foto (dihapus 2026-10-26 10.00)/b.txt"))
	}
	// Di dalam tempat sampah: permanen.
	if err := del("#recycle/foto/2026/a.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "#recycle/foto/2026/a.jpg")); !os.IsNotExist(err) {
		t.Fatal("hapus di tempat sampah harus permanen")
	}
	if _, err := os.Lstat(filepath.Join(root, "#recycle/#recycle")); !os.IsNotExist(err) {
		t.Fatal("tempat sampah tidak boleh masuk tempat sampah")
	}
	// #recycle ditukar symlink ke luar: tidak diikuti, file tetap di tempat.
	outside := t.TempDir()
	_ = os.RemoveAll(filepath.Join(root, recycleDir))
	_ = os.Symlink(outside, filepath.Join(root, recycleDir))
	_ = os.WriteFile(filepath.Join(root, "c.txt"), []byte("c"), 0o644)
	if err := del("c.txt"); err == nil {
		t.Fatal("symlink #recycle harus ditolak")
	}
	if read("c.txt") != "c" {
		t.Fatal("file tidak boleh hilang saat tempat sampah tidak bisa dipakai")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("tidak boleh menulis di luar folder: %v", ents)
	}
}

func TestCleanRecycle(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, recycleDir)
	outside := filepath.Join(t.TempDir(), "rahasia")
	_ = os.WriteFile(outside, []byte("jangan"), 0o644)
	_ = os.MkdirAll(filepath.Join(bin, "x/y"), 0o755)
	_ = os.MkdirAll(filepath.Join(bin, "kosong"), 0o755)
	_ = os.WriteFile(filepath.Join(bin, "x/y/f.txt"), []byte("12345"), 0o644)
	_ = os.WriteFile(filepath.Join(bin, "z.txt"), []byte("123"), 0o644)
	_ = os.Symlink(outside, filepath.Join(bin, "tautan"))
	far := time.Now().Add(time.Hour)

	// Semua masih baru: tidak ada yang dibuang, isi dihitung.
	st, err := cleanRecycle(root, time.Now().Add(-time.Hour), far)
	if err != nil || !st.Done || st.Removed != 0 || st.Files != 3 || st.Bytes < 8 {
		t.Fatalf("%v %+v", err, st)
	}
	// Lewat lama simpan: semua dibuang, #recycle tetap ada, symlink tidak diikuti.
	st, err = cleanRecycle(root, time.Now().Add(time.Hour), far)
	if err != nil || !st.Done || st.Removed != 3 || st.Files != 0 {
		t.Fatalf("%v %+v", err, st)
	}
	if ents, err := os.ReadDir(bin); err != nil || len(ents) != 0 {
		t.Fatalf("isi tersisa: %v %v", ents, err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "jangan" {
		t.Fatal("file di luar tempat sampah tersentuh")
	}
	// Waktu habis: berhenti, belum selesai.
	_ = os.WriteFile(filepath.Join(bin, "lagi.txt"), []byte("x"), 0o644)
	if st, err := cleanRecycle(root, far, time.Now().Add(-time.Second)); err != nil || st.Done {
		t.Fatalf("tenggat: %v %+v", err, st)
	}
	// Tanpa #recycle: tidak ada yang dikerjakan.
	if st, err := cleanRecycle(t.TempDir(), far, far); err != nil || !st.Done {
		t.Fatalf("tanpa tempat sampah: %v %+v", err, st)
	}
	// #recycle berupa symlink: ditolak.
	r2 := t.TempDir()
	_ = os.Symlink(t.TempDir(), filepath.Join(r2, recycleDir))
	if _, err := cleanRecycle(r2, far, far); err == nil {
		t.Fatal("#recycle symlink harus ditolak")
	}
	// makeRecycleDir: dibuat, dan nama yang dipakai file ditolak.
	r3 := t.TempDir()
	if err := makeRecycleDir(r3); err != nil {
		t.Fatal(err)
	}
	if err := makeRecycleDir(r3); err != nil {
		t.Fatal("sudah ada: bukan galat")
	}
	r4 := t.TempDir()
	_ = os.WriteFile(filepath.Join(r4, recycleDir), nil, 0o644)
	if err := makeRecycleDir(r4); err == nil {
		t.Fatal("#recycle berupa file harus ditolak")
	}
}
