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
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Sapo Drive (ADR 0013): setiap pengguna mendapat folder pribadi, dataset
// <pool>/sapo-home/<nama> (penanda sapo:home=<nama>) milik akunnya dengan
// mode 0700, terpasang di <mount_base>/<pool>/sapo-home/<nama>. Folder
// tidak pernah dihapus karena pengguna hilang dari daftar; penghapusan hanya
// lewat homes.delete.

const (
	homePropName = "sapo:home"
	homesDataset = "sapo-home"
	maxHomeUsers = 10000
)

// parentMarker adalah nilai penanda dataset induk (sapo-home, sapo-apps).
// Tidak boleh "-": properti pengguna ZFS yang tidak diset juga terbaca "-",
// jadi dataset buatan luar Sapo akan lolos. "@" tidak mungkin ada di nama
// pengguna atau ID aplikasi, jadi dataset anak yang mewarisinya juga tidak
// pernah cocok.
const parentMarker = "@induk"

// Homes melayani homes.*. Perubahan memakai kunci pool.
type Homes struct {
	shares  *Shares
	managed func(name string) (uid, gid int, ok bool)
	// owner membaca UID pemilik path tanpa mengikuti symlink (diganti di
	// test).
	owner func(path string) (int, error)
}

// NewHomes menyiapkan Sapo Drive di atas shared folder (zfs, mount_base)
// dan catatan akun sapod.
func NewHomes(shares *Shares, managed func(string) (int, int, bool)) *Homes {
	return &Homes{shares: shares, managed: managed, owner: lstatOwner}
}

func lstatOwner(path string) (int, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return 0, err
	}
	return int(st.Uid), nil
}

func homePath(base, pool, user string) string { return filepath.Join(base, pool, homesDataset, user) }

// ApplyOp mengembalikan handler homes.apply.
func (h *Homes) ApplyOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.HomesApplyParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !poolNamePattern.MatchString(p.Pool) || len(p.Users) > maxHomeUsers || (p.Snapshots != nil && !p.Snapshots.Valid()) {
			return nil, invalidApp("pool atau daftar pengguna tidak valid")
		}
		for _, u := range p.Users {
			// Akun aplikasi (sapo-app-*) tidak pernah punya folder pribadi.
			if !accountNamePattern.MatchString(u.Name) || strings.HasPrefix(u.Name, "sapo-") {
				return nil, invalidApp("pengguna tidak valid: " + clip(u.Name))
			}
		}
		sh := h.shares
		if !sh.p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer sh.p.write.Unlock()
		ctx, cancel := context.WithTimeout(ctx, sh.p.t.CreateTimeout)
		defer cancel()
		if err := h.ensureParent(ctx, p.Pool); err != nil {
			return nil, err
		}
		res := proto.HomesApplyResult{Failures: []string{}}
		for _, u := range p.Users {
			if err := h.ensureHome(ctx, p.Pool, u, p.Snapshots); err != nil {
				res.Failures = append(res.Failures, u.Name+": "+clipTo(err.Error(), maxErrorText))
				sh.p.logWarn("folder pribadi gagal disiapkan", "user", u.Name, "err", err)
			}
		}
		return res, nil
	}
}

// ensureParent membuat <pool>/sapo-home (penanda sapo:home=@induk) bila
// belum ada. Induknya milik root, mode 0711: pengguna hanya bisa masuk ke
// foldernya sendiri, tidak bisa melihat daftar folder lain.
func (h *Homes) ensureParent(ctx context.Context, pool string) error {
	sh := h.shares
	pools, err := sh.p.poolNames(ctx)
	if err != nil {
		return err
	}
	if !pools[pool] {
		return &OpError{Code: proto.ErrNotFound, Message: "pool " + pool + " tidak ditemukan"}
	}
	if props, err := sh.zfsProps(ctx, pool, "mountpoint"); err != nil || props["mountpoint"] != filepath.Join(sh.t.MountBase, pool) {
		return &OpError{Code: proto.ErrConflict, Message: "pool " + pool + " tidak terpasang di tempat Sapo"}
	}
	ds := pool + "/" + homesDataset
	want := filepath.Join(sh.t.MountBase, pool, homesDataset)
	props, err := sh.zfsProps(ctx, ds, "mountpoint", "mounted", homePropName, sharePropName)
	if errors.Is(err, errNoDataset) {
		if err := h.zfs(ctx, []string{"create", "-o", homePropName + "=" + parentMarker, "-o", "devices=off", "-o", "setuid=off"}, ds); err != nil {
			return err
		}
		props, err = sh.zfsProps(ctx, ds, "mountpoint", "mounted", homePropName, sharePropName)
	}
	if err != nil {
		return err
	}
	if props[homePropName] != parentMarker || (props[sharePropName] != "-" && props[sharePropName] != "") || props["mountpoint"] != want || props["mounted"] != "yes" {
		return &OpError{Code: proto.ErrConflict, Message: "dataset " + ds + " sudah ada dan bukan milik Sapo Drive"}
	}
	if fi, err := os.Lstat(want); err != nil || !fi.IsDir() || !sh.isMount(want) {
		return fmt.Errorf("%s belum terpasang di %s", ds, want)
	}
	if err := sh.chown(want, 0, 0); err != nil {
		return err
	}
	return os.Chmod(want, 0o711)
}

// ensureHome menyiapkan folder satu pengguna: dataset bertanda, terpasang
// di tempatnya, milik akun pengguna (0700), dan kuotanya.
func (h *Homes) ensureHome(ctx context.Context, pool string, u proto.HomeUser, snap *proto.SnapshotPolicy) error {
	uid, gid, ok := h.managed(u.Name)
	if !ok {
		return errors.New("bukan akun Sapo")
	}
	sh := h.shares
	ds := pool + "/" + homesDataset + "/" + u.Name
	want := homePath(sh.t.MountBase, pool, u.Name)
	props, err := sh.zfsProps(ctx, ds, "mountpoint", "mounted", "refquota", homePropName)
	created := false
	if errors.Is(err, errNoDataset) {
		if err := h.zfs(ctx, []string{"create", "-o", homePropName + "=" + u.Name, "-o", "devices=off", "-o", "setuid=off"}, ds); err != nil {
			return err
		}
		sh.p.logInfo("folder pribadi dibuat", "dataset", ds)
		created = true
		props, err = sh.zfsProps(ctx, ds, "mountpoint", "mounted", "refquota", homePropName)
	}
	if err != nil {
		return err
	}
	if props[homePropName] != u.Name || props["mountpoint"] != want || props["mounted"] != "yes" {
		return fmt.Errorf("dataset %s bukan folder pribadi Sapo atau belum terpasang", ds)
	}
	if fi, err := os.Lstat(want); err != nil || !fi.IsDir() || !sh.isMount(want) {
		return fmt.Errorf("%s belum terpasang di %s", ds, want)
	}
	// Folder yang sudah ada hanya dipakai bila memang milik akun ini: folder
	// sisa pengguna lama bernama sama (misalnya di pool yang baru diimpor
	// lagi) tidak pernah diserahkan ke akun baru. UID tidak pernah dipakai
	// ulang (ADR 0006).
	if !created {
		cur, err := h.owner(want)
		if err != nil {
			return err
		}
		if cur != uid {
			return fmt.Errorf("pemilik folder tidak cocok (UID %d, akun %d); folder ini mungkin milik pengguna lama bernama sama", cur, uid)
		}
	}
	// Lchown tidak mengikuti symlink; path sudah dipastikan direktori, dan
	// induknya milik root (0711), jadi tidak bisa ditukar pengguna.
	if err := sh.chown(want, uid, gid); err != nil {
		return err
	}
	if err := os.Chmod(want, 0o700); err != nil {
		return err
	}
	quota := "none"
	if u.QuotaBytes > 0 {
		quota = strconv.FormatUint(u.QuotaBytes, 10)
	}
	// refquota: snapshot (terjadwal, menyusul) tidak memakan kuota pengguna.
	cur := props["refquota"]
	if cur == "0" || cur == "-" {
		cur = "none"
	}
	if cur != quota {
		if err := h.zfs(ctx, []string{"set", "refquota=" + quota}, ds); err != nil {
			return err
		}
	}
	// Jadwal snapshot yang gagal diatur tidak menahan folder pribadi.
	if err := sh.setSnapPolicy(ctx, ds, snap); err != nil {
		sh.p.logWarn("jadwal snapshot folder pribadi gagal diatur", "dataset", ds, "err", err)
	}
	return nil
}

func (h *Homes) zfs(ctx context.Context, flags []string, ds string) error {
	res, err := h.shares.p.st.run(ctx, h.shares.t.Zfs, flags, ds)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("zfs %s: %s", flags[0], firstLine(res.Stderr))
	}
	return nil
}

// StatusOp mengembalikan handler homes.status: semua folder pribadi di
// semua pool (baca saja).
func (h *Homes) StatusOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		sh := h.shares
		ctx, cancel := context.WithTimeout(ctx, sh.p.st.t.Timeout)
		defer cancel()
		res, err := sh.p.st.run(ctx, sh.t.Zfs, []string{"list", "-H", "-p", "-t", "filesystem", "-o", "name,used,avail,refquota," + homePropName})
		if errors.Is(err, errToolMissing) {
			return []proto.HomeStatus{}, nil
		}
		if err != nil {
			return nil, err
		}
		out := []proto.HomeStatus{}
		sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
		for sc.Scan() {
			f := strings.Split(sc.Text(), "\t")
			if len(f) != 5 {
				continue
			}
			// Hanya <pool>/sapo-home/<nama> dengan penanda yang cocok.
			parts := strings.Split(f[0], "/")
			if len(parts) != 3 || parts[1] != homesDataset || parts[2] != f[4] || !accountNamePattern.MatchString(f[4]) {
				continue
			}
			used, _ := strconv.ParseUint(f[1], 10, 64)
			avail, _ := strconv.ParseUint(f[2], 10, 64)
			quota, _ := strconv.ParseUint(f[3], 10, 64)
			out = append(out, proto.HomeStatus{Pool: parts[0], User: parts[2], UsedBytes: used, AvailableBytes: avail, QuotaBytes: quota})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Pool != out[j].Pool {
				return out[i].Pool < out[j].Pool
			}
			return out[i].User < out[j].User
		})
		return out, nil
	}
}

// DeleteOp mengembalikan handler homes.delete: menghapus folder pribadi
// beserta isinya. Destruktif; sapo-api hanya mengizinkannya untuk pengguna
// yang sudah dihapus (nama diketik ulang, password admin, log audit), dan
// sapod menolaknya selama akun Linux pengguna itu masih ada.
func (h *Homes) DeleteOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.HomeDeleteParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !poolNamePattern.MatchString(p.Pool) || !accountNamePattern.MatchString(p.User) {
			return nil, invalidApp("pool atau pengguna tidak valid")
		}
		// Akun Linux bernama itu masih ada (atau tidak bisa dipastikan):
		// ditolak.
		users, err := readPasswd(h.shares.t.PasswdFile)
		if err != nil {
			return nil, err
		}
		if _, exists := users[p.User]; exists {
			return nil, &OpError{Code: proto.ErrConflict, Message: "pengguna " + p.User + " masih ada; folder pribadinya tidak bisa dihapus"}
		}
		if _, _, ok := h.managed(p.User); ok {
			return nil, &OpError{Code: proto.ErrConflict, Message: "pengguna " + p.User + " masih ada; folder pribadinya tidak bisa dihapus"}
		}
		sh := h.shares
		if !sh.p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer sh.p.write.Unlock()
		ctx, cancel := context.WithTimeout(ctx, sh.p.t.CreateTimeout)
		defer cancel()
		ds := p.Pool + "/" + homesDataset + "/" + p.User
		props, err := sh.zfsProps(ctx, ds, "type", homePropName)
		if errors.Is(err, errNoDataset) || (err == nil && (props["type"] != "filesystem" || props[homePropName] != p.User)) {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "folder pribadi " + ds + " tidak ditemukan"}
		}
		if err != nil {
			return nil, err
		}
		// Dataset anak tidak ikut terhapus diam-diam.
		res, err := sh.p.st.run(ctx, sh.t.Zfs, []string{"list", "-H", "-r", "-t", "filesystem,volume", "-o", "name"}, ds)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != ds {
			return nil, &OpError{Code: proto.ErrConflict, Message: "folder pribadi " + ds + " berisi dataset lain"}
		}
		// Koneksi SMB ke folder itu (layanan [homes] bernama pengguna) ditutup.
		_, _ = sh.p.st.run(ctx, sh.t.Smbcontrol, []string{"smbd", "close-share"}, p.User)
		res, err = sh.p.st.run(ctx, sh.t.Zfs, []string{"unmount"}, ds)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			msg := clipTo(strings.TrimSpace(string(res.Stderr)), maxErrorText)
			code := "home_delete_failed"
			if strings.Contains(strings.ToLower(msg), "busy") {
				code = proto.ErrPoolBusy
			}
			return nil, &OpError{Code: code, Message: msg}
		}
		sh.p.logInfo("menghapus folder pribadi", "dataset", ds)
		res, err = sh.p.st.run(ctx, sh.t.Zfs, []string{"destroy", "-r"}, ds)
		if err == nil && res.ExitCode != 0 {
			err = errors.New(clipTo(strings.TrimSpace(string(res.Stderr)), maxErrorText))
		}
		if err != nil {
			_, _ = sh.p.st.run(ctx, sh.t.Zfs, []string{"mount"}, ds)
			return nil, &OpError{Code: "home_delete_failed", Message: err.Error()}
		}
		sh.p.logInfo("folder pribadi dihapus", "dataset", ds)
		return map[string]bool{"ok": true}, nil
	}
}
