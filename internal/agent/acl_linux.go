package agent

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// applyACLTree memasang ACL folder ke seluruh isinya. Setiap direktori dan
// file dibuka relatif terhadap direktori induknya dengan O_NOFOLLOW, lalu
// ACL dipasang lewat fd (fsetxattr). Jadi symlink, atau direktori yang
// ditukar dengan symlink oleh pengguna SMB saat proses berjalan, tidak
// pernah diikuti keluar dari folder. File selain direktori dan file biasa
// (FIFO, perangkat, socket) dilewati.
func applyACLTree(ctx context.Context, root string, acl shareACL) error {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("membuka %s: %w", root, err)
	}
	dirACL := encodeACL(acl.entries(true))
	fileRW := encodeACL(acl.entries(true))
	fileNoExec := encodeACL(acl.entries(false))
	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	var walk func(fd int, depth int)
	walk = func(fd int, depth int) {
		defer unix.Close(fd)
		if ctx.Err() != nil {
			return
		}
		if err := unix.Fsetxattr(fd, "system.posix_acl_access", dirACL, 0); err != nil {
			note(err)
		}
		if err := unix.Fsetxattr(fd, "system.posix_acl_default", dirACL, 0); err != nil {
			note(err)
		}
		if depth > 256 {
			note(errors.New("folder terlalu dalam"))
			return
		}
		// Daftar isi dibaca dari salinan fd (os.File menutup fd-nya sendiri).
		dup, err := unix.Dup(fd)
		if err != nil {
			note(err)
			return
		}
		dir := os.NewFile(uintptr(dup), "")
		names, err := dir.Readdirnames(-1)
		dir.Close()
		if err != nil {
			note(err)
			return
		}
		for _, name := range names {
			if ctx.Err() != nil {
				return
			}
			child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
			if err != nil {
				// Symlink (ELOOP) sengaja tidak diikuti; yang lain dicatat.
				if !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENXIO) {
					note(err)
				}
				continue
			}
			var st unix.Stat_t
			if err := unix.Fstat(child, &st); err != nil {
				unix.Close(child)
				note(err)
				continue
			}
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				walk(child, depth+1)
			case unix.S_IFREG:
				// Izin jalankan hanya dipertahankan untuk file yang memang
				// bisa dijalankan pemiliknya.
				data := fileNoExec
				if st.Mode&0o100 != 0 {
					data = fileRW
				}
				if err := unix.Fsetxattr(child, "system.posix_acl_access", data, 0); err != nil {
					note(err)
				}
				unix.Close(child)
			default:
				unix.Close(child)
			}
		}
	}
	walk(fd, 0)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return firstErr
}
