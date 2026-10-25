package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// resetOwnership membuat semua isi root milik root:root tanpa ACL POSIX,
// tanpa mengikuti symlink dan tanpa keluar dari sistem berkas root. Dipakai
// setelah memulihkan data dari NAS lain atau cloud: UID dan ACL di salinan
// bisa milik orang lain di NAS ini. Izin folder lalu ditetapkan ulang oleh
// shares.apply (ACL shared folder).
func resetOwnership(root string) error {
	var rootDev uint64
	if st, err := os.Lstat(root); err != nil {
		return err
	} else if s, ok := st.Sys().(*unix.Stat_t); ok {
		rootDev = uint64(s.Dev)
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if s, ok := info.Sys().(*unix.Stat_t); ok && uint64(s.Dev) != rootDev {
			return filepath.SkipDir
		}
		if err := os.Lchown(path, 0, 0); err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		// setuid/setgid tidak berarti apa pun di sini (dataset setuid=off),
		// tetapi dibuang juga.
		if m := info.Mode(); m&(fs.ModeSetuid|fs.ModeSetgid) != 0 && !d.IsDir() {
			if err := os.Chmod(path, m.Perm()); err != nil {
				return err
			}
		}
		for _, attr := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
			if err := unix.Lremovexattr(path, attr); err != nil && !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.EOPNOTSUPP) {
				return err
			}
		}
		return nil
	})
}
