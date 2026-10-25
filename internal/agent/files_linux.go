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
	req     fsRequest
	dev     uint64
	files   int
	bytes   int64
	skipped int
}

func (r *restoreState) count(n int64) error {
	r.files++
	r.bytes += n
	if r.files > r.req.MaxFiles || r.bytes > r.req.MaxBytes {
		return &OpError{Code: "restore_too_large", Message: "terlalu besar untuk dipulihkan sekaligus; pulihkan sebagian-sebagian"}
	}
	return nil
}

// fsRestore menyalin req.Path dari akar snapshot src ke tempat yang sama di
// akar folder root, sebagai pengguna (izin dari kernel). Hasilnya selalu
// nama baru, kecuali Overwrite dan sumber serta tujuannya file biasa.
// Folder disalin ke direktori sementara dulu dan baru diberi nama akhir
// setelah lengkap; bila gagal, salinan setengah jadi dihapus.
func fsRestore(src, root int, req fsRequest) (any, error) {
	var srcSt unix.Stat_t
	if err := unix.Fstat(src, &srcSt); err != nil {
		return nil, err
	}
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
	rs := &restoreState{req: req, dev: srcSt.Dev}
	parent, name := splitParent(req.Path)
	out := proto.FilesRestoreResult{}
	err = withDir(root, parent, func(pfd int) error {
		dest, overwrite, err := restoreName(pfd, name, kind == unix.S_IFREG && req.Overwrite, req.Label)
		if err != nil {
			return err
		}
		tmp, err := tempName()
		if err != nil {
			return err
		}
		if kind == unix.S_IFREG {
			fd, err := openBeneath(src, req.Path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
			if err != nil {
				return err
			}
			err = rs.copyFile(fd, pfd, tmp)
			unix.Close(fd)
			if err == nil {
				flags := uint(unix.RENAME_NOREPLACE)
				if overwrite {
					if err = replaceableFile(pfd, dest); err == nil {
						flags = 0
					}
				}
				if err == nil {
					err = unix.Renameat2(pfd, tmp, pfd, dest, flags)
				}
			}
			if err != nil {
				_ = unix.Unlinkat(pfd, tmp, 0)
				return err
			}
		} else {
			dfd, err := openBeneath(src, req.Path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			err = rs.copyDir(dfd, pfd, tmp, 0)
			unix.Close(dfd)
			if err == nil {
				err = unix.Renameat2(pfd, tmp, pfd, dest, unix.RENAME_NOREPLACE)
			}
			if err != nil {
				_ = removeAt(pfd, tmp, 0)
				return err
			}
		}
		out.Name = dest
		return nil
	})
	out.Files, out.Bytes, out.Skipped = rs.files, rs.bytes, rs.skipped
	return out, err
}

// restoreName memilih nama tujuan: nama asli bila belum ada (atau boleh
// ditimpa), bila tidak "<nama> (<label>)<ekstensi>", lalu " 2", " 3", ….
func restoreName(pfd int, name string, mayOverwrite bool, label string) (string, bool, error) {
	exists := func(n string) (bool, uint32, error) {
		var st unix.Stat_t
		err := unix.Fstatat(pfd, n, &st, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			return false, 0, nil
		}
		return err == nil, st.Mode & unix.S_IFMT, err
	}
	ok, kind, err := exists(name)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return name, false, nil
	}
	if mayOverwrite && kind == unix.S_IFREG {
		return name, true, nil
	}
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
			return "", false, &OpError{Code: proto.ErrInvalidParams, Message: "nama terlalu panjang untuk dipulihkan"}
		}
		ok, _, err := exists(cand)
		if err != nil {
			return "", false, err
		}
		if !ok {
			return cand, false, nil
		}
	}
	return "", false, &OpError{Code: errFileExists, Message: "terlalu banyak hasil pemulihan bernama sama"}
}

func tempName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return uploadTempPrefix + hex.EncodeToString(b), nil
}

// copyFile menyalin isi fd ke file baru name di dirfd (O_EXCL). Izin file
// baru mengikuti ACL bawaan folder tujuan (bukan izin lama di snapshot);
// waktu ubahnya dipertahankan.
func (r *restoreState) copyFile(fd, dirfd int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return &OpError{Code: proto.ErrInvalidParams, Message: "bukan file biasa"}
	}
	if err := r.count(st.Size); err != nil {
		return err
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, 0); err != nil {
		return err
	}
	out, err := openBeneath(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o666)
	if err != nil {
		return err
	}
	// fd sumber dimiliki pemanggil (tidak dibungkus os.File, yang menutup fd
	// lewat finalizer).
	dst := os.NewFile(uintptr(out), "dst")
	_, err = io.Copy(onlyWriter{dst}, io.LimitReader(fdReader(fd), st.Size+1))
	if err == nil {
		err = unix.Futimes(out, []unix.Timeval{unix.NsecToTimeval(st.Atim.Nano()), unix.NsecToTimeval(st.Mtim.Nano())})
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return err
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

// onlyWriter menyembunyikan ReaderFrom os.File (salinan biasa, dalam batas
// LimitReader).
type onlyWriter struct{ w io.Writer }

func (o onlyWriter) Write(p []byte) (int, error) { return o.w.Write(p) }

// copyDir menyalin isi direktori sfd ke direktori baru name di dirfd.
// Symlink, berkas khusus, dan sistem berkas lain di dalamnya dilewati.
func (r *restoreState) copyDir(sfd, dirfd int, name string, depth int) error {
	if depth > 256 {
		return &OpError{Code: proto.ErrInvalidParams, Message: "folder terlalu dalam"}
	}
	if err := r.count(0); err != nil {
		return err
	}
	if err := unix.Mkdirat(dirfd, name, 0o777); err != nil {
		return err
	}
	dfd, err := openBeneath(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	dup, err := unix.Dup(sfd)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(dup), "dir")
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, n := range names {
		if strings.HasPrefix(n, uploadTempPrefix) {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(sfd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch {
		case st.Dev != r.dev:
			r.skipped++
		case st.Mode&unix.S_IFMT == unix.S_IFDIR:
			cfd, err := openBeneath(sfd, n, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			err = r.copyDir(cfd, dfd, n, depth+1)
			unix.Close(cfd)
			if err != nil {
				return err
			}
		case st.Mode&unix.S_IFMT == unix.S_IFREG:
			cfd, err := openBeneath(sfd, n, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
			if err != nil {
				return err
			}
			err = r.copyFile(cfd, dfd, n)
			unix.Close(cfd)
			if err != nil {
				return err
			}
		default:
			r.skipped++
		}
	}
	return nil
}
