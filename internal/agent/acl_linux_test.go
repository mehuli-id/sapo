package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestApplyACLTreeDoesNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "folder")
	outside := filepath.Join(base, "rahasia.txt")
	for _, d := range []string{root, filepath.Join(root, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(outside, []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(root, "sub", "foto.jpg"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "skrip.sh"), []byte("x"), 0o755)
	_ = os.Symlink(outside, filepath.Join(root, "tautan"))
	_ = os.Symlink(base, filepath.Join(root, "sub", "keluar"))
	if err := syscall.Mkfifo(filepath.Join(root, "pipa"), 0o644); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Getuid())
	err := applyACLTree(context.Background(), root, shareACL{ReadUsers: []uint32{uid}})
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		t.Skip("sistem berkas tanpa ACL POSIX")
	}
	if err != nil {
		t.Fatal(err)
	}
	has := func(path, attr string) bool {
		_, err := unix.Getxattr(path, attr, make([]byte, 256))
		return err == nil
	}
	for _, p := range []string{root, filepath.Join(root, "sub"), filepath.Join(root, "sub", "foto.jpg"), filepath.Join(root, "skrip.sh")} {
		if !has(p, "system.posix_acl_access") {
			t.Errorf("%s tanpa ACL", p)
		}
	}
	if !has(filepath.Join(root, "sub"), "system.posix_acl_default") {
		t.Error("subfolder tanpa ACL bawaan")
	}
	// Target symlink di luar folder tidak tersentuh.
	if has(outside, "system.posix_acl_access") || has(base, "system.posix_acl_default") {
		t.Fatal("symlink diikuti keluar folder")
	}
	// File biasa tanpa izin jalankan tetap tanpa izin jalankan.
	st, _ := os.Stat(filepath.Join(root, "sub", "foto.jpg"))
	if st.Mode().Perm()&0o111 != 0 {
		t.Fatalf("foto.jpg menjadi bisa dijalankan: %v", st.Mode())
	}
}
