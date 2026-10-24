package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/proto"
)

// ShareTools adalah program dan nilai Samba untuk shared folder
// (agent.shares di sapo.yaml).
type ShareTools struct {
	Zfs, Setfacl, Testparm, Smbcontrol string
	SMBConf                            string
	Workgroup, ServerString            string
	MinProtocol                        string
}

// Shares melayani shares.apply, shares.delete, dan shares.usage. Perubahan
// memakai kunci yang sama dengan operasi pool, agar folder tidak dibuat atau
// dihapus saat pool sedang dibuat, diganti disknya, atau dihapus.
type Shares struct {
	p *Pools
	t ShareTools
	// chown mengganti pemilik folder (diganti di test, yang tidak berjalan
	// sebagai root).
	chown func(path string, uid, gid int) error
}

// NewShares menyiapkan operasi shared folder di atas pool p.
func NewShares(p *Pools, t ShareTools) *Shares { return &Shares{p: p, t: t, chown: os.Lchown} }

// shareNamePattern: nama folder (juga nama share SMB dan nama dataset).
var shareNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Nama share yang punya arti khusus di Samba.
var reservedShares = []string{"global", "homes", "printers", "print", "ipc"}

// validShareText: keterangan folder masuk ke smb.conf (comment = …), jadi
// hanya teks satu baris tanpa karakter kendali, format, atau tanda kurung siku.
func validShareText(s string) bool {
	if utf8.RuneCountInString(s) > 64 || !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '[' || r == ']' || r == '\\' || r == '%'
	})
}

// principal memeriksa "user:nama" atau "group:nama".
func principal(p string) (kind, name string, ok bool) {
	kind, name, found := strings.Cut(p, ":")
	if !found || (kind != "user" && kind != "group") || !accountNamePattern.MatchString(name) {
		return "", "", false
	}
	return kind, name, true
}

func (s *Shares) checkSpecs(specs []proto.ShareSpec) error {
	invalid := func(msg string) error { return &OpError{Code: proto.ErrInvalidParams, Message: msg} }
	if len(specs) > 1000 {
		return invalid("terlalu banyak shared folder")
	}
	seen := map[string]bool{}
	for _, sp := range specs {
		if !shareNamePattern.MatchString(sp.Name) || slices.Contains(reservedShares, sp.Name) || seen[sp.Name] {
			return invalid("nama shared folder tidak valid: " + clip(sp.Name))
		}
		seen[sp.Name] = true
		if !poolNamePattern.MatchString(sp.Pool) || !validShareText(sp.Description) {
			return invalid("pool atau keterangan shared folder " + sp.Name + " tidak valid")
		}
		if len(sp.Read)+len(sp.Write) > 1000 {
			return invalid("terlalu banyak izin di " + sp.Name)
		}
		for _, p := range append(slices.Clone(sp.Read), sp.Write...) {
			if _, _, ok := principal(p); !ok {
				return invalid("izin tidak valid di " + sp.Name + ": " + clip(p))
			}
		}
	}
	return nil
}

// ApplyOp mengembalikan handler shares.apply: membuat dataset yang belum
// ada, memasang izin folder, lalu menulis ulang smb.conf untuk semua folder.
// Folder yang tidak ada di daftar TIDAK dihapus (penghapusan hanya lewat
// shares.delete); folder itu hanya tidak lagi dibagikan.
func (s *Shares) ApplyOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.SharesApplyParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		if err := s.checkSpecs(req.Shares); err != nil {
			return nil, err
		}
		if !s.p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer s.p.write.Unlock()
		ctx, cancel := context.WithTimeout(ctx, s.p.t.CreateTimeout)
		defer cancel()

		pools, err := s.p.poolNames(ctx)
		if err != nil {
			return nil, err
		}
		out := proto.SharesApplyResult{Shares: []proto.ShareState{}}
		var active []shareConf
		for _, sp := range req.Shares {
			if !pools[sp.Pool] {
				out.Shares = append(out.Shares, proto.ShareState{Name: sp.Name, Pool: sp.Pool, Error: "pool tidak ditemukan"})
				continue
			}
			path, err := s.ensureDataset(ctx, sp)
			if err == nil {
				err = s.setPermissions(ctx, path, sp)
			}
			st := proto.ShareState{Name: sp.Name, Pool: sp.Pool, Path: path}
			if err != nil {
				st.Error = clipTo(err.Error(), maxErrorText)
				s.p.logWarn("shared folder gagal disiapkan", "folder", sp.Name, "err", err)
			} else if len(sp.Read)+len(sp.Write) > 0 {
				// Folder tanpa izin sama sekali tidak dibagikan (valid users
				// kosong di Samba berarti semua pengguna boleh masuk).
				active = append(active, shareConf{spec: sp, path: path})
			}
			out.Shares = append(out.Shares, st)
		}
		out.SMB, err = s.writeSMBConf(ctx, active)
		if err != nil {
			return nil, err
		}
		return out, nil
	}
}

// ensureDataset membuat <pool>/<nama> bila belum ada dan mengembalikan
// mountpoint-nya.
func (s *Shares) ensureDataset(ctx context.Context, sp proto.ShareSpec) (string, error) {
	ds := sp.Pool + "/" + sp.Name
	res, err := s.p.st.run(ctx, s.t.Zfs, []string{"list", "-H", "-o", "name"}, ds)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		res, err = s.p.st.run(ctx, s.t.Zfs, []string{"create"}, ds)
		if err != nil {
			return "", err
		}
		if res.ExitCode != 0 {
			return "", fmt.Errorf("zfs create: %s", firstLine(res.Stderr))
		}
		s.p.logInfo("dataset shared folder dibuat", "dataset", ds)
	}
	res, err = s.p.st.run(ctx, s.t.Zfs, []string{"get", "-H", "-o", "value", "mountpoint"}, ds)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(res.Stdout))
	if res.ExitCode != 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return "", fmt.Errorf("mountpoint %s tidak bisa dipakai: %q", ds, clip(path))
	}
	// Harus direktori sungguhan (bukan symlink) yang sedang terpasang.
	if st, err := os.Lstat(path); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s belum terpasang di %s", ds, path)
	}
	return path, nil
}

// setPermissions: pemilik root, grup akun Sapo, mode 0770, dan ACL POSIX:
// izin tulis = rwx, izin baca = r-x, pengguna lain tanpa akses. ACL bawaan
// (default) membuat file baru di dalamnya mewarisi izin yang sama.
func (s *Shares) setPermissions(ctx context.Context, path string, sp proto.ShareSpec) error {
	// Lchown tidak mengikuti symlink; path sudah dipastikan direktori.
	if err := s.chown(path, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o770); err != nil {
		return err
	}
	entries := []string{"u::rwx", "g::---", "o::---", "m::rwx"}
	add := func(list []string, perm string) {
		for _, p := range list {
			kind, name, _ := principal(p)
			tag := map[string]string{"user": "u", "group": "g"}[kind]
			entries = append(entries, tag+":"+name+":"+perm)
		}
	}
	add(sp.Read, "r-x")
	add(sp.Write, "rwx")
	acl := entries
	for _, e := range entries {
		acl = append(acl, "d:"+e)
	}
	res, err := s.p.st.run(ctx, s.t.Setfacl, []string{"--physical", "--set", strings.Join(acl, ",")}, path)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("setfacl: %s", firstLine(res.Stderr))
	}
	return nil
}

type shareConf struct {
	spec proto.ShareSpec
	path string
}

// renderSMBConf menyusun smb.conf dari templat tetap. Semua nilai sudah
// divalidasi (nama, path, keterangan), jadi tidak bisa menambah baris atau
// bagian baru.
func (s *Shares) renderSMBConf(shares []shareConf) []byte {
	var b bytes.Buffer
	b.WriteString("# Dibuat oleh sapod (Sapo). Jangan diubah manual: atur shared folder lewat panel.\n")
	fmt.Fprintf(&b, "[global]\n")
	for _, kv := range [][2]string{
		{"workgroup", s.t.Workgroup},
		{"server string", s.t.ServerString},
		{"server role", "standalone server"},
		{"server min protocol", s.t.MinProtocol},
		{"security", "user"},
		{"map to guest", "never"},
		{"restrict anonymous", "2"},
		{"usershare max shares", "0"},
		{"load printers", "no"},
		{"printing", "bsd"},
		{"printcap name", "/dev/null"},
		{"disable spoolss", "yes"},
		{"unix password sync", "no"},
		{"logging", "file"},
		{"log file", "/var/log/samba/log.%m"},
		{"max log size", "1000"},
	} {
		fmt.Fprintf(&b, "   %s = %s\n", kv[0], kv[1])
	}
	for _, sc := range shares {
		var valid, write []string
		name := func(p string) string {
			kind, n, _ := principal(p)
			if kind == "group" {
				return "+" + n // grup Unix
			}
			return n
		}
		for _, p := range sc.spec.Read {
			valid = append(valid, name(p))
		}
		for _, p := range sc.spec.Write {
			valid = append(valid, name(p))
			write = append(write, name(p))
		}
		fmt.Fprintf(&b, "\n[%s]\n", sc.spec.Name)
		if sc.spec.Description != "" {
			fmt.Fprintf(&b, "   comment = %s\n", sc.spec.Description)
		}
		fmt.Fprintf(&b, "   path = %s\n", sc.path)
		b.WriteString("   browseable = yes\n   guest ok = no\n   read only = yes\n   inherit acls = yes\n")
		fmt.Fprintf(&b, "   valid users = %s\n", strings.Join(valid, " "))
		if len(write) > 0 {
			fmt.Fprintf(&b, "   write list = %s\n", strings.Join(write, " "))
		}
	}
	return b.Bytes()
}

// writeSMBConf menulis smb.conf (diperiksa testparm dulu) lalu meminta Samba
// memuat ulang. Hasil: "applied", atau "unavailable" bila Samba tidak
// terpasang.
func (s *Shares) writeSMBConf(ctx context.Context, shares []shareConf) (string, error) {
	if _, err := os.Stat(s.t.Testparm); err != nil {
		return "unavailable", nil
	}
	data := s.renderSMBConf(shares)
	dir := filepath.Dir(s.t.SMBConf)
	if err := checkSocketDir(dir); err != nil {
		return "", fmt.Errorf("direktori smb.conf tidak aman: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".smb.conf-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	res, err := s.p.st.run(ctx, s.t.Testparm, []string{"--suppress-prompt", "-s"}, tmp.Name())
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("testparm menolak smb.conf: %s", firstLine(res.Stderr))
	}
	if err := os.Rename(tmp.Name(), s.t.SMBConf); err != nil {
		return "", err
	}
	// smbd yang belum berjalan tidak perlu dimuat ulang.
	_, _ = s.p.st.run(ctx, s.t.Smbcontrol, []string{"smbd", "reload-config"})
	return "applied", nil
}

// DeleteOp mengembalikan handler shares.delete: menghapus dataset folder
// beserta isinya (dan snapshot-nya). Destruktif; sapo-api sudah meminta
// password dan nama diketik ulang, dan mencatatnya di log audit.
func (s *Shares) DeleteOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.ShareDeleteParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		if !shareNamePattern.MatchString(req.Name) || !poolNamePattern.MatchString(req.Pool) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama tidak valid"}
		}
		if !s.p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer s.p.write.Unlock()
		ctx, cancel := context.WithTimeout(ctx, s.p.t.CreateTimeout)
		defer cancel()
		ds := req.Pool + "/" + req.Name
		// Harus sistem berkas yang benar-benar ada, tepat di bawah pool.
		res, err := s.p.st.run(ctx, s.t.Zfs, []string{"list", "-H", "-o", "name,type"}, ds)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != ds+"\tfilesystem" {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "shared folder " + ds + " tidak ditemukan"}
		}
		// Koneksi SMB yang masih membuka folder ditutup dulu.
		_, _ = s.p.st.run(ctx, s.t.Smbcontrol, []string{"smbd", "close-share"}, req.Name)
		s.p.logInfo("menghapus shared folder", "dataset", ds)
		res, err = s.p.st.run(ctx, s.t.Zfs, []string{"destroy", "-r"}, ds)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			msg := clipTo(strings.TrimSpace(string(res.Stderr)), maxErrorText)
			code := "share_delete_failed"
			if strings.Contains(strings.ToLower(msg), "busy") {
				code = proto.ErrPoolBusy
			}
			return nil, &OpError{Code: code, Message: msg}
		}
		s.p.logInfo("shared folder dihapus", "dataset", ds)
		return map[string]bool{"ok": true}, nil
	}
}

// UsageOp mengembalikan handler shares.usage: ruang terpakai dan tersedia
// setiap dataset folder (baca saja).
func (s *Shares) UsageOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, s.p.st.t.Timeout)
		defer cancel()
		res, err := s.p.st.run(ctx, s.t.Zfs, []string{"list", "-H", "-p", "-t", "filesystem", "-o", "name,used,avail"})
		if errors.Is(err, errToolMissing) {
			return map[string]proto.ShareUsage{}, nil
		}
		if err != nil {
			return nil, err
		}
		out := map[string]proto.ShareUsage{}
		sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
		for sc.Scan() {
			f := strings.Split(sc.Text(), "\t")
			// Hanya <pool>/<folder> (satu tingkat di bawah pool).
			if len(f) != 3 || strings.Count(f[0], "/") != 1 {
				continue
			}
			used, _ := strconv.ParseUint(f[1], 10, 64)
			avail, _ := strconv.ParseUint(f[2], 10, 64)
			out[f[0]] = proto.ShareUsage{UsedBytes: used, AvailableBytes: avail}
		}
		return out, nil
	}
}
