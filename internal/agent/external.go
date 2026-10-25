package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Penyimpanan eksternal (ADR 0015): drive USB berformat exFAT, FAT, atau
// NTFS dipasang sebagai folder eksternal. Format itu tidak menyimpan pemilik
// dan izin Linux, jadi izin ditentukan saat dipasang: pemilik root, grup
// Sapo yang dipilih admin, baca saja atau baca-tulis untuk grup itu, dan
// selalu nosuid,nodev,noexec. Sapo tidak pernah memformat atau mengubah isi
// drive sendiri. Folder yang diatur diingat per UUID sistem berkas: dipasang
// lagi saat drive dicolok atau NAS menyala, dan dilepas (lazy) bila drive
// dicabut tanpa dilepas.

// ExternalTools mengatur penyimpanan eksternal (agent.external di sapo.yaml).
type ExternalTools struct {
	Mount, Umount, Lsblk string
	// MountBase: folder eksternal terpasang di <MountBase>/<nama>.
	MountBase string
	StateFile string
	GroupFile string
	// Rentang ID grup Sapo (agent.accounts): hanya grup Sapo yang boleh
	// diberi akses.
	IDMin, IDMax int
	Timeout      time.Duration
}

// External melayani external.*.
type External struct {
	t      ExternalTools
	st     *Storage
	shares *Shares
	mu     sync.Mutex
	// cache: daftar volume terakhir (diperbarui setiap pemeriksaan).
	cache []proto.ExternalVolume
	// mount, isMount diganti di test.
	isMount func(path string) bool
}

// NewExternal menyiapkan penyimpanan eksternal.
func NewExternal(t ExternalTools, st *Storage, shares *Shares) *External {
	e := &External{t: t, st: st, shares: shares, isMount: isMountPoint}
	shares.external = e
	return e
}

// Format yang bisa dipasang dan tipe mount-nya.
var externalFS = map[string]string{"exfat": "exfat", "vfat": "vfat", "ntfs": "ntfs3"}

var uuidPattern = regexp.MustCompile(`^[A-Za-z0-9-]{4,64}$`)

type externalConf struct {
	UUID     string `json:"uuid"`
	FSType   string `json:"fstype"`
	ReadOnly bool   `json:"read_only"`
	Group    string `json:"group"`
	SMB      bool   `json:"smb"`
}

type externalState struct {
	Volumes map[string]externalConf `json:"volumes"` // nama → konfigurasi
}

func (e *External) load() (externalState, error) {
	st := externalState{Volumes: map[string]externalConf{}}
	data, err := os.ReadFile(e.t.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("catatan penyimpanan eksternal rusak (%s): %w", e.t.StateFile, err)
	}
	if st.Volumes == nil {
		st.Volumes = map[string]externalConf{}
	}
	return st, nil
}

func (e *External) save(st externalState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(e.t.StateFile, data)
}

func (e *External) mountPath(name string) string { return filepath.Join(e.t.MountBase, name) }

// lsblkExternal: kolom lsblk untuk drive USB (termasuk UUID sistem berkas).
var lsblkExternalFlags = []string{
	"--json", "--bytes", "--tree",
	"--output", "NAME,PATH,TYPE,SIZE,MODEL,VENDOR,SERIAL,WWN,TRAN,ROTA,RM,FSTYPE,PTTYPE,LABEL,UUID,MOUNTPOINTS",
}

type extDevice struct {
	lsblkDevice
	UUID     *string     `json:"uuid"`
	Children []extDevice `json:"children"`
}

type extVolumeRaw struct {
	vol  proto.ExternalVolume
	path string
	// mounts: titik pasang saat ini (dari lsblk).
	mounts []string
}

// scan membaca volume di drive USB.
func (e *External) scan(ctx context.Context) ([]extVolumeRaw, error) {
	ctx, cancel := context.WithTimeout(ctx, e.t.Timeout)
	defer cancel()
	res, err := e.st.run(ctx, e.t.Lsblk, lsblkExternalFlags)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("lsblk: %s", firstLine(res.Stderr))
	}
	var out struct {
		BlockDevices []extDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		return nil, fmt.Errorf("keluaran lsblk tidak bisa dibaca: %w", err)
	}
	var vols []extVolumeRaw
	for _, d := range out.BlockDevices {
		if d.Type != "disk" || str(d.Tran) != "usb" || !deviceNamePattern.MatchString(d.Name) {
			continue
		}
		add := func(v extDevice) {
			fstype := str(v.FSType)
			if fstype == "" {
				return
			}
			r := extVolumeRaw{path: v.Path, vol: proto.ExternalVolume{
				UUID: str(v.UUID), Device: v.Name, Disk: d.Name, Model: str(d.Model), Serial: str(d.Serial),
				FSType: fstype, Label: str(v.Label), SizeBytes: uint64(v.Size), State: "available",
			}}
			for _, m := range v.Mountpoints {
				if m != nil && *m != "" {
					r.mounts = append(r.mounts, *m)
				}
			}
			_, ok := externalFS[fstype]
			switch {
			case fstype == "zfs_member" || strings.HasSuffix(fstype, "_raid_member") || fstype == "LVM2_member" || fstype == "crypto_LUKS" || fstype == "swap":
				r.vol.Reason = "member" // anggota pool, RAID, LVM, terenkripsi, atau swap
			case !ok:
				r.vol.Reason = "unsupported_fs"
			case !uuidPattern.MatchString(r.vol.UUID):
				r.vol.Reason = "no_uuid"
			case len(r.mounts) > 0 && !slices.ContainsFunc(r.mounts, func(m string) bool { return strings.HasPrefix(m, e.t.MountBase+"/") }):
				r.vol.Reason = "mounted_elsewhere"
			default:
				r.vol.Supported = true
			}
			vols = append(vols, r)
		}
		if len(d.Children) == 0 {
			add(d)
		}
		for _, c := range d.Children {
			if c.Type == "part" {
				add(c)
			}
		}
	}
	return vols, nil
}

// List membaca volume sekarang, memasang folder yang diatur dan tercolok,
// melepas folder yang drive-nya hilang, lalu memperbarui cache.
func (e *External) refresh(ctx context.Context) ([]proto.ExternalVolume, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	vols, err := e.scan(ctx)
	if err != nil {
		return nil, false, err
	}
	st, err := e.load()
	if err != nil {
		return nil, false, err
	}
	byUUID := map[string]*extVolumeRaw{}
	for i := range vols {
		if vols[i].vol.UUID != "" {
			byUUID[vols[i].vol.UUID] = &vols[i]
		}
	}
	changed := false
	var out []proto.ExternalVolume
	seen := map[string]bool{}
	names := make([]string, 0, len(st.Volumes))
	for n := range st.Volumes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		c := st.Volumes[name]
		mp := e.mountPath(name)
		r, present := byUUID[c.UUID]
		mounted := e.isMount(mp)
		v := proto.ExternalVolume{UUID: c.UUID, FSType: c.FSType, Name: name, ReadOnly: c.ReadOnly, Group: c.Group, SMB: c.SMB}
		switch {
		case !present:
			if mounted {
				// Drive dicabut tanpa dilepas: titik pasang yang mati dilepas.
				e.umount(ctx, mp, true)
				changed = true
			}
			v.State = "disconnected"
		case !mounted:
			v = mergeVol(r.vol, v)
			if err := e.mountVol(ctx, name, c, r.path); err != nil {
				v.State, v.Error = "error", clipTo(err.Error(), maxErrorText)
			} else {
				v.State = "mounted"
				changed = true
			}
		default:
			v = mergeVol(r.vol, v)
			v.State = "mounted"
		}
		if present {
			seen[c.UUID] = true
		}
		if v.State == "mounted" {
			var sfs syscall.Statfs_t
			if syscall.Statfs(mp, &sfs) == nil {
				v.UsedBytes = (sfs.Blocks - sfs.Bfree) * uint64(sfs.Bsize)
				v.AvailableBytes = sfs.Bavail * uint64(sfs.Bsize)
			}
		}
		out = append(out, v)
	}
	for _, r := range vols {
		if !seen[r.vol.UUID] || r.vol.UUID == "" {
			out = append(out, r.vol)
		}
	}
	e.cache = out
	return out, changed, nil
}

// mergeVol: info drive (r) dengan konfigurasi folder (c).
func mergeVol(r, c proto.ExternalVolume) proto.ExternalVolume {
	r.Name, r.ReadOnly, r.Group, r.SMB = c.Name, c.ReadOnly, c.Group, c.SMB
	r.Supported, r.Reason = true, ""
	return r
}

// gid: GID grup Sapo name (di rentang ID grup Sapo).
func (e *External) gid(name string) (int, error) {
	groups, err := readGroup(e.t.GroupFile)
	if err != nil {
		return 0, err
	}
	g, ok := groups[name]
	if !ok || g.GID < e.t.IDMin || g.GID > e.t.IDMax {
		return 0, &OpError{Code: proto.ErrInvalidParams, Message: "grup " + clip(name) + " bukan grup Sapo"}
	}
	return g.GID, nil
}

// mountVol memasang volume dengan opsi tetap (tidak pernah dari panel).
func (e *External) mountVol(ctx context.Context, name string, c externalConf, dev string) error {
	fstype, ok := externalFS[c.FSType]
	if !ok {
		return &OpError{Code: proto.ErrInvalidParams, Message: "format tidak didukung"}
	}
	gid, err := e.gid(c.Group)
	if err != nil {
		return err
	}
	// Grup: rwx (atau r-x bila baca saja); lainnya tanpa akses.
	fmask, dmask := "0117", "0007"
	opts := []string{"nosuid", "nodev", "noexec", "uid=0", "gid=" + strconv.Itoa(gid)}
	if c.ReadOnly {
		fmask, dmask = "0137", "0027"
		opts = append(opts, "ro")
	}
	opts = append(opts, "fmask="+fmask, "dmask="+dmask)
	if fstype == "vfat" {
		opts = append(opts, "utf8", "flush")
	}
	mp := e.mountPath(name)
	if err := os.MkdirAll(e.t.MountBase, 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(mp, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if fi, err := os.Lstat(mp); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s bukan direktori", mp)
	}
	ctx, cancel := context.WithTimeout(ctx, e.t.Timeout)
	defer cancel()
	res, err := e.st.run(ctx, e.t.Mount, []string{"-t", fstype, "-o", strings.Join(opts, ",")}, dev, mp)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("mount: %s", firstLine(res.Stderr))
	}
	return nil
}

// umount melepas titik pasang (lazy hanya untuk drive yang sudah hilang),
// lalu membuang direktorinya bila kosong.
func (e *External) umount(ctx context.Context, mp string, lazy bool) error {
	flags := []string{}
	if lazy {
		flags = append(flags, "-l")
	}
	ctx, cancel := context.WithTimeout(ctx, e.t.Timeout)
	defer cancel()
	res, err := e.st.run(ctx, e.t.Umount, flags, mp)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		msg := firstLine(res.Stderr)
		if strings.Contains(strings.ToLower(msg), "busy") {
			return &OpError{Code: proto.ErrPoolBusy, Message: "drive masih dibuka (File Station, SMB, atau aplikasi); tutup dulu lalu coba lagi"}
		}
		return fmt.Errorf("umount: %s", msg)
	}
	_ = os.Remove(mp)
	return nil
}

// Run memeriksa drive secara berkala (colok/cabut) sampai ctx selesai.
func (e *External) Run(ctx context.Context) {
	for {
		if _, changed, err := e.refresh(ctx); err == nil && changed {
			e.shares.refreshSMB(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// mounted: folder eksternal yang sedang terpasang (untuk smb.conf dan File
// Station), dari cache.
func (e *External) mounted() []proto.ExternalVolume {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []proto.ExternalVolume
	for _, v := range e.cache {
		if v.State == "mounted" && v.Name != "" {
			out = append(out, v)
		}
	}
	return out
}

// root: akar folder eksternal name bila terpasang (File Station).
func (e *External) root(name string) (string, bool) {
	for _, v := range e.mounted() {
		if v.Name == name {
			mp := e.mountPath(name)
			return mp, e.isMount(mp)
		}
	}
	return "", false
}

// --- Handler -----------------------------------------------------------------

// ListOp mengembalikan handler external.list.
func (e *External) ListOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.ExternalListParams
		if len(raw) > 0 && string(raw) != "null" {
			if err := decodeParams(raw, &p); err != nil {
				return nil, err
			}
		}
		if p.Cached {
			out := e.mounted()
			if out == nil {
				out = []proto.ExternalVolume{}
			}
			return out, nil
		}
		vols, changed, err := e.refresh(ctx)
		if err != nil {
			return nil, err
		}
		if changed {
			e.shares.refreshSMB(ctx)
		}
		if vols == nil {
			vols = []proto.ExternalVolume{}
		}
		return vols, nil
	}
}

// MountOp mengembalikan handler external.mount.
func (e *External) MountOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.ExternalMountParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !shareNamePattern.MatchString(p.Name) || slices.Contains(reservedShares, p.Name) || !uuidPattern.MatchString(p.UUID) || !accountNamePattern.MatchString(p.Group) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama, drive, atau grup tidak valid"}
		}
		if _, err := e.gid(p.Group); err != nil {
			return nil, err
		}
		e.mu.Lock()
		vols, err := e.scan(ctx)
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		st, err := e.load()
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		if _, taken := st.Volumes[p.Name]; taken {
			e.mu.Unlock()
			return nil, &OpError{Code: proto.ErrConflict, Message: "nama folder eksternal sudah dipakai"}
		}
		var vol *extVolumeRaw
		for i := range vols {
			if vols[i].vol.UUID == p.UUID {
				vol = &vols[i]
			}
		}
		for _, c := range st.Volumes {
			if c.UUID == p.UUID {
				e.mu.Unlock()
				return nil, &OpError{Code: proto.ErrConflict, Message: "drive ini sudah dipasang"}
			}
		}
		switch {
		case vol == nil:
			e.mu.Unlock()
			return nil, &OpError{Code: proto.ErrNotFound, Message: "drive tidak ditemukan; colok lalu coba lagi"}
		case !vol.vol.Supported:
			e.mu.Unlock()
			return nil, &OpError{Code: "external_unsupported", Message: "drive ini tidak bisa dipasang (" + vol.vol.Reason + ")"}
		}
		c := externalConf{UUID: p.UUID, FSType: vol.vol.FSType, ReadOnly: p.ReadOnly, Group: p.Group, SMB: p.SMB}
		if err := e.mountVol(ctx, p.Name, c, vol.path); err != nil {
			_ = os.Remove(e.mountPath(p.Name))
			e.mu.Unlock()
			return nil, err
		}
		st.Volumes[p.Name] = c
		err = e.save(st)
		e.mu.Unlock()
		if err != nil {
			_ = e.umount(ctx, e.mountPath(p.Name), false)
			return nil, err
		}
		_, _, _ = e.refresh(ctx)
		e.shares.refreshSMB(ctx)
		return map[string]bool{"ok": true}, nil
	}
}

// UnmountOp mengembalikan handler external.unmount: koneksi SMB ditutup,
// drive dilepas (data ditulis tuntas), dan konfigurasinya dilupakan. Bila
// masih dipakai, tidak ada yang berubah.
func (e *External) UnmountOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.ExternalParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !shareNamePattern.MatchString(p.Name) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama tidak valid"}
		}
		e.mu.Lock()
		st, err := e.load()
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		c, ok := st.Volumes[p.Name]
		if !ok {
			e.mu.Unlock()
			return nil, &OpError{Code: proto.ErrNotFound, Message: "folder eksternal tidak ditemukan"}
		}
		mp := e.mountPath(p.Name)
		if e.isMount(mp) {
			if c.SMB {
				_, _ = e.st.run(ctx, e.shares.t.Smbcontrol, []string{"smbd", "close-share"}, p.Name)
			}
			if err := e.umount(ctx, mp, false); err != nil {
				e.mu.Unlock()
				return nil, err
			}
		} else {
			_ = os.Remove(mp)
		}
		delete(st.Volumes, p.Name)
		err = e.save(st)
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}
		_, _, _ = e.refresh(ctx)
		e.shares.refreshSMB(ctx)
		return map[string]bool{"ok": true}, nil
	}
}
