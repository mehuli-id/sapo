package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/proto"
	"golang.org/x/sys/unix"
)

// FSHelperMain adalah "sapod fs-helper": satu operasi file sebagai akun
// pengguna (sudah diatur sapod lewat setuid/setgid/setgroups sebelum
// program ini berjalan). Permintaan di stdin, jawaban di stdout, file
// descriptor (open_read/open_write) lewat unix socket di fd 3.
func FSHelperMain(stdin io.Reader, stdout io.Writer) int {
	if os.Geteuid() == 0 {
		// Tidak pernah berjalan sebagai root: izin harus dari kernel.
		_ = json.NewEncoder(stdout).Encode(fsReply{Code: proto.ErrInternal, Message: "fs-helper tidak boleh berjalan sebagai root"})
		return 1
	}
	unix.Umask(0o007)
	var req fsRequest
	if err := json.NewDecoder(io.LimitReader(stdin, 64<<10)).Decode(&req); err != nil {
		_ = json.NewEncoder(stdout).Encode(fsReply{Code: proto.ErrInvalidParams, Message: "permintaan tidak valid"})
		return 1
	}
	result, err := fsDo(req)
	reply := fsReply{}
	if err != nil {
		reply.Code, reply.Message = fsErrorCode(err), err.Error()
	} else if result != nil {
		reply.Result, _ = json.Marshal(result)
	}
	_ = json.NewEncoder(stdout).Encode(reply)
	return 0
}

// fsErrorCode menerjemahkan errno ke kode galat untuk panel.
func fsErrorCode(err error) string {
	var oe *OpError
	switch {
	case errors.As(err, &oe):
		return oe.Code
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR):
		return proto.ErrNotFound
	case errors.Is(err, unix.EEXIST):
		return errFileExists
	case errors.Is(err, unix.ENOTEMPTY):
		return errFileNotEmpty
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM), errors.Is(err, unix.EROFS):
		return errFilePermission
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EXDEV):
		return proto.ErrInvalidParams
	}
	return proto.ErrInternal
}

// resolveFlags: path tidak boleh keluar dari akar, tidak mengikuti symlink
// (termasuk komponen terakhir), dan tidak masuk sistem berkas lain
// (snapshot di .zfs, mount lain).
const resolveFlags = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV

func openBeneath(dirfd int, path string, flags int, mode uint32) (int, error) {
	if path == "" {
		path = "."
	}
	return unix.Openat2(dirfd, path, &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC), Mode: uint64(mode), Resolve: resolveFlags})
}

// splitParent: "a/b/c" → ("a/b", "c"); "c" → ("", "c").
func splitParent(p string) (string, string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

func fsDo(req fsRequest) (any, error) {
	root, err := unix.Open(req.Root, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(root)
	switch req.Op {
	case "list":
		return fsList(root, req.Path, req.Max)
	case "stat":
		// O_PATH: tidak membaca isi dan tidak butuh izin baca file itu
		// sendiri (sama dengan ls di direktori induk).
		fd, err := openBeneath(root, req.Path, unix.O_PATH|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		defer unix.Close(fd)
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return nil, err
		}
		_, name := splitParent(req.Path)
		e := proto.FileEntry{Name: name, Size: st.Size, ModTime: time.Unix(st.Mtim.Unix()).UTC(), Type: "other"}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			e.Type, e.Size = "dir", 0
		case unix.S_IFREG:
			e.Type = "file"
		}
		return e, nil
	case "mkdir":
		parent, name := splitParent(req.Path)
		return nil, withDir(root, parent, func(pfd int) error { return unix.Mkdirat(pfd, name, 0o777) })
	case "rename":
		fromDir, fromName := splitParent(req.Path)
		toDir, toName := splitParent(req.To)
		return nil, withDir(root, fromDir, func(ofd int) error {
			return withDir(root, toDir, func(nfd int) error {
				flags := uint(unix.RENAME_NOREPLACE)
				if req.Overwrite {
					// Menimpa (atomik) hanya file biasa yang boleh ditulis
					// pengguna, sama dengan commit.
					if err := replaceableFile(nfd, toName); err != nil {
						return err
					}
					flags = 0
				}
				return unix.Renameat2(ofd, fromName, nfd, toName, flags)
			})
		})
	case "delete":
		parent, name := splitParent(req.Path)
		if req.Recycle == recycleDir && !inRecycle(req.Path) {
			return nil, withDir(root, parent, func(pfd int) error { return moveToRecycle(root, pfd, parent, name, req.Label) })
		}
		return nil, withDir(root, parent, func(pfd int) error { return removeAt(pfd, name, 0) })
	case "open_read":
		fd, err := openBeneath(root, req.Path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
		if err != nil {
			return nil, err
		}
		defer unix.Close(fd)
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return nil, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "bukan file biasa"}
		}
		// O_NONBLOCK hanya untuk membuka (FIFO tidak menggantung).
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, 0); err != nil {
			return nil, err
		}
		if err := sendFD(fd); err != nil {
			return nil, err
		}
		return proto.FilesOpenResult{Size: st.Size, ModTime: time.Unix(st.Mtim.Unix())}, nil
	case "open_write":
		var out proto.FilesOpenResult
		err := withDir(root, req.Path, func(pfd int) error {
			cleanStaleUploads(pfd)
			b := make([]byte, 8)
			if _, err := rand.Read(b); err != nil {
				return err
			}
			out.Temp = uploadTempPrefix + hex.EncodeToString(b)
			fd, err := openBeneath(pfd, out.Temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o666)
			if err != nil {
				return err
			}
			defer unix.Close(fd)
			return sendFD(fd)
		})
		return out, err
	case "restore":
		if req.Src == "" || req.Path == "" {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "pemulihan tidak valid"}
		}
		src, err := unix.Open(req.Src, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		defer unix.Close(src)
		return fsRestore(src, root, req)
	case "commit", "abort":
		if !validTemp(req.Temp) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "unggahan tidak valid"}
		}
		parent, name := splitParent(req.Path)
		return nil, withDir(root, parent, func(pfd int) error {
			if req.Op == "abort" {
				return unix.Unlinkat(pfd, req.Temp, 0)
			}
			flags := uint(unix.RENAME_NOREPLACE)
			if req.Overwrite {
				if err := replaceableFile(pfd, name); err != nil {
					return err
				}
				flags = 0
			}
			return unix.Renameat2(pfd, req.Temp, pfd, name, flags)
		})
	}
	return nil, &OpError{Code: proto.ErrInvalidParams, Message: "operasi tidak dikenal"}
}

// replaceableFile: name (bila ada) adalah file biasa, bukan direktori atau
// symlink, dan boleh ditulis pengguna (bukan sekadar direktorinya yang boleh
// ditulis).
func replaceableFile(dirfd int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return &OpError{Code: errFileExists, Message: "nama sudah dipakai folder atau symlink"}
	}
	// Sudah pasti file biasa (bukan symlink); tanpa flag agar kernel
	// memeriksa ACL sungguhan.
	return unix.Faccessat(dirfd, name, unix.W_OK, 0)
}

// withDir membuka direktori path (di bawah akar) lalu menjalankan fn.
func withDir(root int, path string, fn func(fd int) error) error {
	fd, err := openBeneath(root, path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return fn(fd)
}

func fsList(root int, path string, max int) (proto.FilesListResult, error) {
	out := proto.FilesListResult{Entries: []proto.FileEntry{}}
	fd, err := openBeneath(root, path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return out, err
	}
	dir := os.NewFile(uintptr(fd), "dir")
	defer dir.Close()
	names, err := dir.Readdirnames(max + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return out, err
	}
	if len(names) > max {
		names, out.Truncated = names[:max], true
	}
	slices.Sort(names)
	// Jawaban harus muat di satu pesan sapod (1 MiB).
	budget := proto.MaxMessageBytes * 3 / 4
	for _, n := range names {
		if strings.HasPrefix(n, uploadTempPrefix) || (path == "" && n == ".zfs") {
			continue
		}
		if budget -= len(n) + 96; budget < 0 {
			out.Truncated = true
			break
		}
		var st unix.Stat_t
		if err := unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue
		}
		e := proto.FileEntry{Name: n, Size: st.Size, ModTime: time.Unix(st.Mtim.Unix()).UTC()}
		switch {
		case !utf8.ValidString(n):
			// Nama yang bukan UTF-8 (misalnya dari klien lama) tidak bisa
			// disebut ulang dengan tepat dari panel: tampil saja.
			e.Type = "other"
		case st.Mode&unix.S_IFMT == unix.S_IFDIR:
			e.Type, e.Size = "dir", 0
		case st.Mode&unix.S_IFMT == unix.S_IFREG:
			e.Type = "file"
		case st.Mode&unix.S_IFMT == unix.S_IFLNK:
			e.Type = "symlink"
		default:
			e.Type = "other"
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

// removeAt menghapus name di direktori dirfd, beserta isinya bila
// direktori. Symlink dihapus, tidak diikuti; sistem berkas lain tidak
// dimasuki.
func removeAt(dirfd int, name string, depth int) error {
	if depth > 256 {
		return &OpError{Code: proto.ErrInvalidParams, Message: "folder terlalu dalam"}
	}
	unlinkErr := unix.Unlinkat(dirfd, name, 0)
	if unlinkErr == nil || !(errors.Is(unlinkErr, unix.EISDIR) || errors.Is(unlinkErr, unix.EPERM)) {
		return unlinkErr
	}
	fd, err := openBeneath(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if errors.Is(err, unix.ENOTDIR) {
		return unlinkErr // file yang tidak boleh dihapus (EPERM)
	}
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), "dir")
	defer dir.Close()
	// Dibaca ulang dari awal sampai kosong: menghapus sambil membaca bisa
	// membuat sebagian nama terlewat.
	for {
		if _, err := unix.Seek(fd, 0, 0); err != nil {
			return err
		}
		names, rerr := dir.Readdirnames(512)
		if len(names) == 0 {
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return rerr
			}
			break
		}
		for _, n := range names {
			if err := removeAt(fd, n, depth+1); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
	}
	return unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR)
}

// moveToRecycle memindah name (di pfd, folder parent) ke #recycle/parent
// di akar folder (ADR 0021), membuat folder perantara bila perlu. Nama yang
// sudah ada di tempat sampah tidak ditimpa: diberi akhiran label.
func moveToRecycle(root, pfd int, parent, name, label string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(pfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	dfd, err := openOrMkdir(root, recycleDir)
	if err != nil {
		return err
	}
	if parent != "" {
		for _, c := range strings.Split(parent, "/") {
			nfd, err := openOrMkdir(dfd, c)
			unix.Close(dfd)
			if err != nil {
				return err
			}
			dfd = nfd
		}
	}
	defer unix.Close(dfd)
	dest, _, err := restoreName(dfd, name, false, label)
	if err != nil {
		return err
	}
	return unix.Renameat2(pfd, name, dfd, dest, unix.RENAME_NOREPLACE)
}

// openOrMkdir membuka folder name di dfd, membuatnya dulu bila belum ada.
func openOrMkdir(dfd int, name string) (int, error) {
	fd, err := openBeneath(dfd, name, unix.O_PATH|unix.O_DIRECTORY, 0)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(dfd, name, 0o777); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, err
		}
		fd, err = openBeneath(dfd, name, unix.O_PATH|unix.O_DIRECTORY, 0)
	}
	return fd, err
}

// fdSocket: unix socket ke sapod (fd 3 dari ExtraFiles; diganti di test).
var fdSocket = 3

// cleanStaleUploads membuang file unggahan sementara yang tertinggal lebih
// dari sehari (sapo-api atau sapod berhenti di tengah unggahan).
func cleanStaleUploads(dirfd int) {
	fd, err := unix.Dup(dirfd)
	if err != nil {
		return
	}
	dir := os.NewFile(uintptr(fd), "dir")
	defer dir.Close()
	names, _ := dir.Readdirnames(-1)
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	for _, n := range names {
		var st unix.Stat_t
		if validTemp(n) && unix.Fstatat(dirfd, n, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG && st.Mtim.Sec < cutoff {
			_ = unix.Unlinkat(dirfd, n, 0)
		}
	}
}

// sendFD mengirim fd ke sapod.
func sendFD(fd int) error {
	return unix.Sendmsg(fdSocket, []byte{0}, unix.UnixRights(fd), nil, 0)
}

func socketPair() (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "fs-parent"), os.NewFile(uintptr(fds[1]), "fs-child"), nil
}

// recvFile menerima satu fd dari proses pembantu (sudah selesai, jadi
// pesannya sudah menunggu di socket).
func recvFile(sock *os.File) (*os.File, error) {
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, flags, _, err := unix.Recvmsg(int(sock.Fd()), buf, oob, unix.MSG_DONTWAIT|unix.MSG_CMSG_CLOEXEC)
	if err != nil {
		return nil, err
	}
	if flags&unix.MSG_CTRUNC != 0 {
		return nil, errors.New("pesan file dari proses pembantu terpotong")
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		return nil, errors.New("proses pembantu tidak mengirim file")
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		for _, fd := range fds {
			unix.Close(fd)
		}
		return nil, errors.New("proses pembantu mengirim file tidak valid")
	}
	return os.NewFile(uintptr(fds[0]), "file"), nil
}

// --- Pemulihan dari snapshot (ADR 0014) --------------------------------------

// restoreState menghitung batas satu pemulihan.
type restoreState struct {
	req      fsRequest
	dev      uint64
	deadline time.Time
	files    int
	bytes    int64
	skipped  int
}

// count menghitung satu entri (file, folder, atau yang dilewati) dan
// ukurannya terhadap batas dan tenggat.
func (r *restoreState) count(n int64) error {
	r.files++
	r.bytes += n
	if r.files > r.req.MaxFiles || r.bytes > r.req.MaxBytes {
		return &OpError{Code: "restore_too_large", Message: "terlalu besar untuk dipulihkan sekaligus; pulihkan sebagian-sebagian"}
	}
	return r.inTime()
}

func (r *restoreState) inTime() error {
	if time.Now().After(r.deadline) {
		return &OpError{Code: "timeout", Message: "pemulihan terlalu lama; pulihkan sebagian-sebagian"}
	}
	return nil
}

// readNames membaca isi direktori fd sedikit demi sedikit.
func readNames(fd int, fn func(name string) error) error {
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(dup), "dir")
	defer dir.Close()
	for {
		names, err := dir.Readdirnames(512)
		for _, n := range names {
			if ferr := fn(n); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(names) == 0) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// sameIno: name di dirfd masih entri yang dibuat pemulihan ini (bukan
// ditukar pengguna lain selama penyalinan).
func sameIno(dirfd int, name string, dev, ino uint64) bool {
	var st unix.Stat_t
	return unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Dev == dev && st.Ino == ino
}

// fsRestore menyalin req.Path dari akar snapshot src ke tempat yang sama di
// akar folder root, sebagai pengguna (izin dari kernel).
//   - Ukurannya dihitung dulu: yang melebihi batas, ruang kosong, atau
//     tenggat ditolak sebelum menyalin.
//   - Hasilnya nama baru bila nama itu sudah ada. Bila Overwrite (file
//     biasa), file sekarang tidak hilang: diberi nama "sebelum dipulihkan"
//     dulu.
//   - Salinan dibuat dengan nama sementara lalu baru diberi nama akhir.
//     Entri sementara dikenali lewat dev/ino dan dibersihkan lewat fd yang
//     dipegang, jadi pengguna lain yang menukar namanya tidak bisa membuat
//     pemulihan menghapus atau mengganti folder lain.
func fsRestore(src, root int, req fsRequest) (any, error) {
	var srcSt unix.Stat_t
	if err := unix.Fstat(src, &srcSt); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(270 * time.Second)
	if req.Deadline > 0 {
		deadline = time.Unix(req.Deadline, 0)
	}
	rs := &restoreState{req: req, dev: srcSt.Dev, deadline: deadline}
	sfd, err := openBeneath(src, req.Path, unix.O_PATH|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	err = unix.Fstat(sfd, &st)
	unix.Close(sfd)
	if err != nil {
		return nil, err
	}
	kind := st.Mode & unix.S_IFMT
	if kind != unix.S_IFDIR && kind != unix.S_IFREG {
		return nil, &OpError{Code: proto.ErrInvalidParams, Message: "hanya file dan folder yang bisa dipulihkan"}
	}
	isFile := kind == unix.S_IFREG
	// Hitung dulu (snapshot tidak berubah).
	if isFile {
		err = rs.count(st.Size)
	} else {
		var dfd int
		if dfd, err = openBeneath(src, req.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0); err == nil {
			err = rs.scan(dfd, 0)
			unix.Close(dfd)
		}
	}
	if err != nil {
		return nil, err
	}
	if req.Avail > 0 && rs.bytes > req.Avail {
		return nil, &OpError{Code: "restore_no_space", Message: "ruang kosong tidak cukup untuk memulihkan"}
	}
	rs.files, rs.bytes, rs.skipped = 0, 0, 0

	parent, name := splitParent(req.Path)
	out := proto.FilesRestoreResult{}
	err = withDir(root, parent, func(pfd int) error {
		dest, replace, err := restoreName(pfd, name, isFile && req.Overwrite, req.Label)
		if err != nil {
			return err
		}
		tmp, err := tempName()
		if err != nil {
			return err
		}
		if isFile {
			fd, err := openBeneath(src, req.Path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
			if err != nil {
				return err
			}
			dev, ino, err := rs.copyFile(fd, pfd, tmp)
			unix.Close(fd)
			if err == nil && !sameIno(pfd, tmp, dev, ino) {
				err = &OpError{Code: errFileExists, Message: "file sementara berubah selama pemulihan"}
			}
			if err == nil {
				err = placeFile(pfd, tmp, dest, replace, req.Replaced)
			}
			if err != nil {
				if sameIno(pfd, tmp, dev, ino) {
					_ = unix.Unlinkat(pfd, tmp, 0)
				}
				return err
			}
		} else {
			if err := unix.Mkdirat(pfd, tmp, 0o777); err != nil {
				return err
			}
			dfd, err := openBeneath(pfd, tmp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			defer unix.Close(dfd)
			var dst unix.Stat_t
			if err := unix.Fstat(dfd, &dst); err != nil {
				return err
			}
			sdfd, err := openBeneath(src, req.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			if err == nil {
				err = rs.copyInto(sdfd, dfd, 0)
				unix.Close(sdfd)
			}
			if err == nil && !sameIno(pfd, tmp, dst.Dev, dst.Ino) {
				err = &OpError{Code: errFileExists, Message: "folder sementara berubah selama pemulihan"}
			}
			if err == nil {
				err = unix.Renameat2(pfd, tmp, pfd, dest, unix.RENAME_NOREPLACE)
			}
			if err != nil {
				// Dibersihkan lewat fd yang dipegang; direktori kosongnya
				// hanya dihapus bila masih entri yang sama.
				clearDir(dfd)
				if sameIno(pfd, tmp, dst.Dev, dst.Ino) {
					_ = unix.Unlinkat(pfd, tmp, unix.AT_REMOVEDIR)
				}
				return err
			}
		}
		out.Name = dest
		return nil
	})
	out.Files, out.Bytes, out.Skipped = rs.files, rs.bytes, rs.skipped
	return out, err
}

// placeFile memberi file sementara tmp nama akhir dest. Bila replace, file
// dest sekarang (file biasa yang boleh ditulis) diberi nama cadangan dulu
// (label replaced), lalu dikembalikan bila langkah berikutnya gagal.
func placeFile(pfd int, tmp, dest string, replace bool, replaced string) error {
	if !replace {
		return unix.Renameat2(pfd, tmp, pfd, dest, unix.RENAME_NOREPLACE)
	}
	if err := replaceableFile(pfd, dest); err != nil {
		return err
	}
	backup, err := freeName(pfd, dest, replaced)
	if err != nil {
		return err
	}
	if err := unix.Renameat2(pfd, dest, pfd, backup, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := unix.Renameat2(pfd, tmp, pfd, dest, unix.RENAME_NOREPLACE); err != nil {
		_ = unix.Renameat2(pfd, backup, pfd, dest, unix.RENAME_NOREPLACE)
		return err
	}
	return nil
}

// clearDir menghapus isi direktori fd (bukan direktorinya).
func clearDir(fd int) {
	var names []string
	_ = readNames(fd, func(n string) error { names = append(names, n); return nil })
	for _, n := range names {
		_ = removeAt(fd, n, 1)
	}
}

// restoreName memilih nama tujuan: nama asli bila belum ada (atau boleh
// ditimpa: replace), bila tidak nama bebas dengan label.
func restoreName(pfd int, name string, mayOverwrite bool, label string) (string, bool, error) {
	var st unix.Stat_t
	err := unix.Fstatat(pfd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(err, unix.ENOENT):
		return name, false, nil
	case err != nil:
		return "", false, err
	case mayOverwrite && st.Mode&unix.S_IFMT == unix.S_IFREG:
		return name, true, nil
	}
	n, err := freeName(pfd, name, label)
	return n, false, err
}

// freeName: "<nama> (<label>)<ekstensi>", lalu " 2", " 3", … yang belum ada.
func freeName(pfd int, name, label string) (string, error) {
	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	for i := 1; i <= 100; i++ {
		cand := stem + " (" + label + ")" + ext
		if i > 1 {
			cand = stem + " (" + label + " " + strconv.Itoa(i) + ")" + ext
		}
		if len(cand) > 255 {
			return "", &OpError{Code: proto.ErrInvalidParams, Message: "nama terlalu panjang untuk dipulihkan"}
		}
		var st unix.Stat_t
		err := unix.Fstatat(pfd, cand, &st, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			return cand, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", &OpError{Code: errFileExists, Message: "terlalu banyak hasil pemulihan bernama sama"}
}

func tempName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return uploadTempPrefix + hex.EncodeToString(b), nil
}

// copyFile menyalin isi fd ke file baru name di dirfd (O_EXCL), sedikit
// demi sedikit (tenggat diperiksa). Izin file baru mengikuti ACL bawaan
// folder tujuan (bukan izin lama di snapshot); waktu ubahnya dipertahankan.
// Mengembalikan dev/ino file baru.
func (r *restoreState) copyFile(fd, dirfd int, name string) (uint64, uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return 0, 0, &OpError{Code: proto.ErrInvalidParams, Message: "bukan file biasa"}
	}
	if err := r.count(st.Size); err != nil {
		return 0, 0, err
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, 0); err != nil {
		return 0, 0, err
	}
	out, err := openBeneath(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o666)
	if err != nil {
		return 0, 0, err
	}
	var ost unix.Stat_t
	if err := unix.Fstat(out, &ost); err != nil {
		unix.Close(out)
		return 0, 0, err
	}
	// fd sumber dimiliki pemanggil (tidak dibungkus os.File, yang menutup fd
	// lewat finalizer).
	dst := os.NewFile(uintptr(out), "dst")
	buf := make([]byte, 1<<20)
	in := io.LimitReader(fdReader(fd), st.Size+1)
	for err == nil {
		var n int
		n, err = in.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				err = werr
			}
		}
		if err == nil {
			err = r.inTime()
		}
	}
	if errors.Is(err, io.EOF) {
		err = unix.Futimes(out, []unix.Timeval{unix.NsecToTimeval(st.Atim.Nano()), unix.NsecToTimeval(st.Mtim.Nano())})
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return ost.Dev, ost.Ino, err
}

// fdReader membaca langsung dari fd (EINTR diulang).
type fdReader int

func (f fdReader) Read(p []byte) (int, error) {
	for {
		n, err := unix.Read(int(f), p)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 && len(p) > 0 {
			return 0, io.EOF
		}
		return n, nil
	}
}

// scan menghitung isi direktori snapshot sfd (tanpa menyalin).
func (r *restoreState) scan(sfd, depth int) error {
	if depth > 256 {
		return &OpError{Code: proto.ErrInvalidParams, Message: "folder terlalu dalam"}
	}
	return readNames(sfd, func(n string) error {
		var st unix.Stat_t
		if err := unix.Fstatat(sfd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch {
		case st.Dev != r.dev || (st.Mode&unix.S_IFMT != unix.S_IFDIR && st.Mode&unix.S_IFMT != unix.S_IFREG):
			return r.count(0)
		case st.Mode&unix.S_IFMT == unix.S_IFREG:
			return r.count(st.Size)
		}
		if err := r.count(0); err != nil {
			return err
		}
		cfd, err := openBeneath(sfd, n, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer unix.Close(cfd)
		return r.scan(cfd, depth+1)
	})
}

// copyInto menyalin isi direktori snapshot sfd ke direktori dfd (milik
// pemulihan ini). Symlink, berkas khusus, dan sistem berkas lain dilewati.
func (r *restoreState) copyInto(sfd, dfd, depth int) error {
	if depth > 256 {
		return &OpError{Code: proto.ErrInvalidParams, Message: "folder terlalu dalam"}
	}
	return readNames(sfd, func(n string) error {
		if strings.HasPrefix(n, uploadTempPrefix) {
			return nil
		}
		var st unix.Stat_t
		if err := unix.Fstatat(sfd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch {
		case st.Dev != r.dev || (st.Mode&unix.S_IFMT != unix.S_IFDIR && st.Mode&unix.S_IFMT != unix.S_IFREG):
			r.skipped++
			return r.count(0)
		case st.Mode&unix.S_IFMT == unix.S_IFREG:
			cfd, err := openBeneath(sfd, n, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
			if err != nil {
				return err
			}
			defer unix.Close(cfd)
			_, _, err = r.copyFile(cfd, dfd, n)
			return err
		}
		if err := r.count(0); err != nil {
			return err
		}
		if err := unix.Mkdirat(dfd, n, 0o777); err != nil {
			return err
		}
		nd, err := openBeneath(dfd, n, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer unix.Close(nd)
		cfd, err := openBeneath(sfd, n, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer unix.Close(cfd)
		return r.copyInto(cfd, nd, depth+1)
	})
}
