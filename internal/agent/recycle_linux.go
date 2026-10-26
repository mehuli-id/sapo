package agent

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// makeRecycleDir membuat <root>/#recycle bila belum ada. Dibuat lewat fd
// akar folder tanpa mengikuti symlink; izinnya diwarisi dari ACL bawaan
// folder. Nama yang sudah dipakai selain folder ditolak.
func makeRecycleDir(root string) error {
	rfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(rfd)
	if err := unix.Mkdirat(rfd, recycleDir, 0o777); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(rfd, recycleDir, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New(recycleDir + " di folder ini bukan folder; ganti namanya agar tempat sampah bisa dipakai")
	}
	return nil
}

var errRecycleDeadline = errors.New("waktu habis")

// cleanRecycle membuang isi <root>/#recycle yang ctime-nya sebelum cutoff
// (file, symlink, dan folder yang sudah kosong), lalu menghitung sisanya.
// Berjalan sebagai root di pohon yang bisa diubah pengguna, jadi setiap nama
// dibuka relatif ke fd folder induknya (openat2 tanpa symlink, tanpa pindah
// sistem berkas) dan dihapus dengan unlinkat; tidak ada path yang
// diikuti. Folder #recycle sendiri tidak dihapus.
func cleanRecycle(root string, cutoff, deadline time.Time) (recycleStats, error) {
	var st recycleStats
	rfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return st, err
	}
	defer unix.Close(rfd)
	fd, err := openBeneath(rfd, recycleDir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if errors.Is(err, unix.ENOENT) {
		st.Done = true
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer unix.Close(fd)
	var self unix.Stat_t
	if err := unix.Fstat(fd, &self); err != nil {
		return st, err
	}
	_, err = st.walk(fd, self.Dev, cutoff, deadline, 0)
	if errors.Is(err, errRecycleDeadline) {
		return st, nil
	}
	st.Done = err == nil
	return st, err
}

// walk membersihkan isi dfd; true bila dfd kini kosong.
func (st *recycleStats) walk(dfd int, dev uint64, cutoff, deadline time.Time, depth int) (bool, error) {
	if depth > 256 {
		return false, nil
	}
	// Nama dikumpulkan dulu: menghapus sambil membaca bisa melewatkan nama.
	var names []string
	if err := readNames(dfd, func(n string) error { names = append(names, n); return nil }); err != nil {
		return false, err
	}
	empty := true
	for _, n := range names {
		if time.Now().After(deadline) {
			return false, errRecycleDeadline
		}
		var s unix.Stat_t
		if err := unix.Fstatat(dfd, n, &s, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if !errors.Is(err, unix.ENOENT) {
				empty = false
			}
			continue
		}
		old := time.Unix(s.Ctim.Unix()).Before(cutoff)
		if s.Mode&unix.S_IFMT == unix.S_IFDIR {
			if s.Dev != dev {
				empty = false
				continue
			}
			fd, err := openBeneath(dfd, n, unix.O_RDONLY|unix.O_DIRECTORY, 0)
			if err != nil {
				empty = false
				continue
			}
			sub, err := st.walk(fd, dev, cutoff, deadline, depth+1)
			unix.Close(fd)
			if err != nil {
				return false, err
			}
			// Folder kosong dibuang bila ia sendiri sudah lama (folder yang
			// dihapus dalam keadaan kosong tetap disimpan selama lama simpan).
			if sub && old && unix.Unlinkat(dfd, n, unix.AT_REMOVEDIR) == nil {
				continue
			}
			empty = false
			continue
		}
		if old && unix.Unlinkat(dfd, n, 0) == nil {
			st.Removed++
			continue
		}
		empty = false
		st.Files++
		st.Bytes += uint64(max(s.Size, 0))
	}
	return empty, nil
}
