package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/proto"
)

// File Station (ADR 0010). sapod tidak pernah membaca atau menulis isi
// shared folder sendiri: setiap operasi dijalankan proses pembantu
// ("sapod fs-helper") sebagai akun Linux pengguna yang login, dengan grup
// akunnya, sehingga izin ditegakkan kernel (ACL folder) persis seperti
// lewat SMB. Path dibuka dengan openat2 di bawah akar folder, tanpa symlink
// dan tanpa pindah sistem berkas.

// FilesTools mengatur operasi files.* (agent.files di sapo.yaml).
type FilesTools struct {
	// Helper: program sapod sendiri (dijalankan dengan argumen fs-helper).
	Helper    string
	MountBase string
	GroupFile string
	// MaxEntries: isi direktori paling banyak yang dikembalikan.
	MaxEntries int
	// Timeout satu operasi; DeleteTimeout untuk menghapus isi folder.
	Timeout, DeleteTimeout time.Duration
	// Rentang ID akun dan grup Sapo (agent.accounts): hanya grup di rentang
	// ini yang ikut menjadi grup proses pembantu.
	IDMin, IDMax int
}

// Files melayani operasi File Station.
type Files struct {
	t      FilesTools
	shares *Shares
	// managed: UID dan GID akun buatan sapod (Accounts.Managed).
	managed func(name string) (uid, gid int, ok bool)
	// run menjalankan proses pembantu (diganti di test).
	run func(ctx context.Context, cred fsCred, req fsRequest, wantFile bool) (fsReply, *os.File, error)
}

// NewFiles menyiapkan File Station. managed wajib (hanya akun Sapo).
func NewFiles(t FilesTools, shares *Shares, managed func(string) (int, int, bool)) *Files {
	f := &Files{t: t, shares: shares, managed: managed}
	f.run = f.runHelper
	return f
}

// fsCred adalah identitas proses pembantu.
type fsCred struct {
	UID, GID int
	Groups   []uint32
}

// fsRequest dikirim ke proses pembantu lewat stdin.
type fsRequest struct {
	Op        string `json:"op"`
	Root      string `json:"root"`
	Path      string `json:"path"`
	To        string `json:"to,omitempty"`
	Temp      string `json:"temp,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
	Max       int    `json:"max,omitempty"`
}

// fsReply adalah jawaban proses pembantu (stdout).
type fsReply struct {
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

// Kode galat File Station (selain kode umum proto).
const (
	errFilePermission = "permission_denied"
	errFileExists     = "file_exists"
	errFileNotEmpty   = "not_empty"
)

// uploadTempPrefix: nama file unggahan yang belum selesai; tidak tampil di
// daftar dan tidak bisa dipakai sebagai nama biasa.
const uploadTempPrefix = ".sapo-upload-"

// cleanFilePath memeriksa path relatif dari panel: dipisah "/", tanpa
// komponen kosong, ".", "..", atau ".zfs" (snapshot), tanpa NUL dan
// karakter kendali, setiap nama paling panjang 255 byte. "" = akar folder.
func cleanFilePath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if len(p) > 4096 || !utf8.ValidString(p) {
		return "", errors.New("path tidak valid")
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." || c == ".zfs" || len(c) > 255 || strings.HasPrefix(c, uploadTempPrefix) ||
			strings.ContainsFunc(c, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return "", fmt.Errorf("nama tidak valid: %s", clip(c))
		}
	}
	return p, nil
}

// prepare memeriksa permintaan: akun Sapo, folder Sapo yang terpasang, dan
// path. Mengembalikan identitas proses pembantu dan akar folder.
func (f *Files) prepare(ctx context.Context, t proto.FilesTarget) (fsCred, string, string, error) {
	invalid := func(msg string) error { return &OpError{Code: proto.ErrInvalidParams, Message: msg} }
	if !accountNamePattern.MatchString(t.User) || !poolNamePattern.MatchString(t.Pool) || !shareNamePattern.MatchString(t.Share) {
		return fsCred{}, "", "", invalid("pengguna atau folder tidak valid")
	}
	path, err := cleanFilePath(t.Path)
	if err != nil {
		return fsCred{}, "", "", invalid(err.Error())
	}
	cred, err := f.cred(t.User)
	if err != nil {
		return fsCred{}, "", "", err
	}
	root := filepath.Join(f.t.MountBase, t.Pool, t.Share)
	props, err := f.shares.zfsProps(ctx, t.Pool+"/"+t.Share, "mountpoint", "mounted", sharePropName)
	if errors.Is(err, errNoDataset) || (err == nil && (props[sharePropName] != t.Share || props["mountpoint"] != root || props["mounted"] != "yes")) {
		return fsCred{}, "", "", &OpError{Code: proto.ErrNotFound, Message: "shared folder tidak ditemukan atau belum terpasang"}
	}
	if err != nil {
		return fsCred{}, "", "", err
	}
	return cred, root, path, nil
}

// cred: UID, GID, dan grup tambahan akun. Hanya akun buatan sapod; grup
// tambahan hanya grup Sapo (rentang ID yang sama dengan grup utama).
func (f *Files) cred(user string) (fsCred, error) {
	uid, gid, ok := f.managed(user)
	if !ok {
		return fsCred{}, &OpError{Code: proto.ErrNotFound, Message: "akun " + user + " bukan akun Sapo"}
	}
	groups, err := readGroup(f.t.GroupFile)
	if err != nil {
		return fsCred{}, err
	}
	c := fsCred{UID: uid, GID: gid, Groups: []uint32{uint32(gid)}}
	for _, g := range groups {
		// Grup sistem (sudo, disk, shadow, …) tidak pernah ikut.
		if slices.Contains(g.Members, user) && g.GID != gid && g.GID >= f.t.IDMin && g.GID <= f.t.IDMax {
			c.Groups = append(c.Groups, uint32(g.GID))
		}
	}
	return c, nil
}

// call menjalankan satu operasi pembantu dan menerjemahkan galatnya.
func (f *Files) call(ctx context.Context, t proto.FilesTarget, req fsRequest, timeout time.Duration, wantFile bool) (json.RawMessage, *os.File, error) {
	cred, root, path, err := f.prepare(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	req.Root, req.Path = root, path
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	reply, file, err := f.run(ctx, cred, req, wantFile)
	if err != nil {
		return nil, nil, err
	}
	if reply.Code != "" {
		if file != nil {
			file.Close()
		}
		return nil, nil, &OpError{Code: reply.Code, Message: clipTo(reply.Message, maxErrorText)}
	}
	if wantFile && file == nil {
		return nil, nil, errors.New("proses pembantu tidak mengirim file")
	}
	return reply.Result, file, nil
}

// runHelper menjalankan "sapod fs-helper" sebagai akun pengguna. File
// descriptor (open_read/open_write) dikirim balik lewat socketpair di fd 3.
func (f *Files) runHelper(ctx context.Context, cred fsCred, req fsRequest, wantFile bool) (fsReply, *os.File, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return fsReply{}, nil, err
	}
	cmd := exec.CommandContext(ctx, f.t.Helper, "fs-helper")
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = proto.MaxMessageBytes, 16<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(cred.UID), Gid: uint32(cred.GID), Groups: cred.Groups},
		Setsid:     true,
	}
	var parent *os.File
	if wantFile {
		var child *os.File
		parent, child, err = socketPair()
		if err != nil {
			return fsReply{}, nil, err
		}
		defer parent.Close()
		cmd.ExtraFiles = []*os.File{child}
		defer child.Close()
	}
	runErr := cmd.Run()
	var reply fsReply
	if len(stdout.Bytes()) > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
			return fsReply{}, nil, fmt.Errorf("jawaban proses pembantu tidak valid: %w", err)
		}
	} else if runErr != nil {
		if ctx.Err() != nil {
			return fsReply{}, nil, &OpError{Code: "timeout", Message: "operasi file melewati batas waktu"}
		}
		return fsReply{}, nil, fmt.Errorf("proses pembantu gagal: %v %s", runErr, firstLine(stderr.Bytes()))
	}
	if !wantFile || reply.Code != "" {
		return reply, nil, nil
	}
	file, err := recvFile(parent)
	return reply, file, err
}

// --- Handler -----------------------------------------------------------------

func (f *Files) target(raw json.RawMessage, v any) error {
	return decodeParams(raw, v)
}

// ListOp mengembalikan handler files.list.
func (f *Files) ListOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var t proto.FilesTarget
		if err := f.target(raw, &t); err != nil {
			return nil, err
		}
		res, _, err := f.call(ctx, t, fsRequest{Op: "list", Max: f.t.MaxEntries}, f.t.Timeout, false)
		if err != nil {
			return nil, err
		}
		var out proto.FilesListResult
		if err := json.Unmarshal(res, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
}

// simpleOp: mkdir dan delete (tanpa hasil selain ok).
func (f *Files) simpleOp(op string) Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var t proto.FilesTarget
		if err := f.target(raw, &t); err != nil {
			return nil, err
		}
		if t.Path == "" {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "akar folder tidak bisa diubah"}
		}
		timeout := f.t.Timeout
		if op == "delete" {
			timeout = f.t.DeleteTimeout
		}
		if _, _, err := f.call(ctx, t, fsRequest{Op: op}, timeout, false); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	}
}

// MkdirOp mengembalikan handler files.mkdir.
func (f *Files) MkdirOp() Handler { return f.simpleOp("mkdir") }

// DeleteOp mengembalikan handler files.delete (direktori beserta isinya).
func (f *Files) DeleteOp() Handler { return f.simpleOp("delete") }

// RenameOp mengembalikan handler files.rename (tidak menimpa).
func (f *Files) RenameOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.FilesRenameParams
		if err := f.target(raw, &p); err != nil {
			return nil, err
		}
		to, err := cleanFilePath(p.To)
		if err != nil || to == "" || p.Path == "" {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama baru tidak valid"}
		}
		if _, _, err := f.call(ctx, p.FilesTarget, fsRequest{Op: "rename", To: to}, f.t.Timeout, false); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	}
}

// OpenReadOp mengembalikan handler files.open_read: file biasa dibuka
// sebagai pengguna, descriptor-nya diteruskan ke sapo-api.
func (f *Files) OpenReadOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var t proto.FilesTarget
		if err := f.target(raw, &t); err != nil {
			return nil, err
		}
		return f.openOp(ctx, t, fsRequest{Op: "open_read"})
	}
}

// OpenWriteOp mengembalikan handler files.open_write: file sementara baru
// di direktori Path, dibuka untuk ditulis.
func (f *Files) OpenWriteOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var t proto.FilesTarget
		if err := f.target(raw, &t); err != nil {
			return nil, err
		}
		return f.openOp(ctx, t, fsRequest{Op: "open_write"})
	}
}

func (f *Files) openOp(ctx context.Context, t proto.FilesTarget, req fsRequest) (any, error) {
	res, file, err := f.call(ctx, t, req, f.t.Timeout, true)
	if err != nil {
		return nil, err
	}
	var out proto.FilesOpenResult
	if err := json.Unmarshal(res, &out); err != nil {
		file.Close()
		return nil, err
	}
	return FileResult{Result: out, File: file}, nil
}

// validTemp: nama file sementara buatan open_write.
func validTemp(s string) bool {
	if !strings.HasPrefix(s, uploadTempPrefix) || len(s) != len(uploadTempPrefix)+16 {
		return false
	}
	for _, r := range s[len(uploadTempPrefix):] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// CommitOp mengembalikan handler files.commit: file sementara (di
// direktori induk Path) diberi nama akhir Path.
func (f *Files) CommitOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.FilesCommitParams
		if err := f.target(raw, &p); err != nil {
			return nil, err
		}
		if !validTemp(p.Temp) || p.Path == "" {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "unggahan tidak valid"}
		}
		if _, _, err := f.call(ctx, p.FilesTarget, fsRequest{Op: "commit", Temp: p.Temp, Overwrite: p.Overwrite}, f.t.Timeout, false); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	}
}

// AbortOp mengembalikan handler files.abort: file sementara dibuang.
func (f *Files) AbortOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.FilesCommitParams
		if err := f.target(raw, &p); err != nil {
			return nil, err
		}
		if !validTemp(p.Temp) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "unggahan tidak valid"}
		}
		if _, _, err := f.call(ctx, p.FilesTarget, fsRequest{Op: "abort", Temp: p.Temp}, f.t.Timeout, false); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	}
}
