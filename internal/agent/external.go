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
	// mu: operasi (pindai, pasang, lepas) satu per satu.
	mu sync.Mutex
	// cacheMu melindungi cache (daftar terakhir) dan tidak pernah ditahan
	// selama program eksternal atau syscall berjalan, jadi File Station tidak
	// ikut menunggu drive yang lambat.
	cacheMu sync.RWMutex
	cache   []proto.ExternalVolume
	// missing: berapa pemeriksaan berturut-turut drive tidak terlihat;
	// retry: kapan pemasangan yang gagal dicoba lagi (per nama).
	missing map[string]int
	retry   map[string]retryState
	// isMount dan rootOK diganti di test.
	isMount func(path string) bool
	rootOK  func(path string, gid int) error
	log     func(msg string, args ...any)
}

type retryState struct {
	next  time.Time
	delay time.Duration
}

// externalMissingScans: drive dianggap dicabut setelah sekian pemeriksaan
// berturut-turut tidak melihatnya (lsblk bisa sesaat tidak memuatnya saat
// USB direset).
const externalMissingScans = 3

// NewExternal menyiapkan penyimpanan eksternal.
func NewExternal(t ExternalTools, st *Storage, shares *Shares) *External {
	e := &External{t: t, st: st, shares: shares, isMount: isMountPoint, rootOK: checkExternalRoot,
		missing: map[string]int{}, retry: map[string]retryState{}, log: shares.p.logInfo}
	shares.external = e
	return e
}

// Format yang bisa dipasang dan tipe mount-nya.
var externalFS = map[string]string{"exfat": "exfat", "vfat": "vfat", "ntfs": "ntfs3"}

// Partisi sistem yang tidak pernah ditawarkan (EFI, Microsoft Reserved,
// Windows Recovery; MBR 0xef = EFI).
var systemPartTypes = map[string]bool{
	"c12a7328-f81f-11d2-ba4b-00a0c93ec93b": true,
	"e3c9e316-0b5c-4db8-817d-f92df00215ae": true,
	"de94bba4-06d1-4d40-a16a-bfd50179d6ac": true,
	"0xef":                                 true,
}

var uuidPattern = regexp.MustCompile(`^[A-Za-z0-9-]{4,64}$`)

// externalConf: konfigurasi folder dan sidik jari drive saat dipasang
// (UUID FAT/exFAT hanya 32 bit dan ikut tersalin saat drive dikloning, jadi
// ukuran dan nomor seri disk juga dicocokkan). GID: grup saat dipasang;
// grup yang dihapus lalu dibuat ulang (GID baru) tidak mewarisi akses.
type externalConf struct {
	UUID      string `json:"uuid"`
	FSType    string `json:"fstype"`
	SizeBytes uint64 `json:"size_bytes"`
	Serial    string `json:"serial"`
	ReadOnly  bool   `json:"read_only"`
	Group     string `json:"group"`
	GID       int    `json:"gid"`
	SMB       bool   `json:"smb"`
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
	"--output", "NAME,PATH,TYPE,SIZE,MODEL,VENDOR,SERIAL,WWN,TRAN,ROTA,RM,FSTYPE,PTTYPE,PARTTYPE,LABEL,UUID,MOUNTPOINTS",
}

type extDevice struct {
	lsblkDevice
	UUID     *string     `json:"uuid"`
	PartType *string     `json:"parttype"`
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
			case systemPartTypes[strings.ToLower(str(v.PartType))]:
				r.vol.Reason = "system_partition"
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
	// UUID yang dipakai lebih dari satu volume (drive hasil kloning) tidak
	// bisa membedakan drive: tidak pernah dipasang.
	count := map[string]int{}
	for _, r := range vols {
		if r.vol.UUID != "" {
			count[r.vol.UUID]++
		}
	}
	for i := range vols {
		if count[vols[i].vol.UUID] > 1 {
			vols[i].vol.Supported, vols[i].vol.Reason = false, "duplicate_uuid"
		}
	}
	return vols, nil
}

// matches: drive sekarang sama dengan yang dipasang dulu (sidik jari).
func (c externalConf) matches(v proto.ExternalVolume) bool {
	return v.Supported && v.FSType == c.FSType && v.SizeBytes == c.SizeBytes && v.Serial == c.Serial
}

// refresh membaca volume sekarang, memasang folder yang diatur dan tercolok,
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
	now := time.Now()
	for _, name := range names {
		c := st.Volumes[name]
		mp := e.mountPath(name)
		r, present := byUUID[c.UUID]
		mounted := e.isMount(mp)
		v := proto.ExternalVolume{UUID: c.UUID, FSType: c.FSType, Name: name, ReadOnly: c.ReadOnly, Group: c.Group, SMB: c.SMB, SizeBytes: c.SizeBytes}
		if present {
			seen[c.UUID] = true
			e.missing[name] = 0
		}
		switch {
		case !present:
			e.missing[name]++
			v.State = "disconnected"
			if mounted && e.missing[name] < externalMissingScans {
				// Mungkin hanya sesaat (USB direset): tunggu pemeriksaan berikutnya.
				v.State = "mounted"
			} else if mounted {
				// Drive dicabut tanpa dilepas: titik pasang yang mati dilepas.
				if err := e.umount(ctx, mp, true); err == nil {
					changed = true
					e.log("drive eksternal dicabut tanpa dilepas; titik pasang dilepas", "folder", name)
				} else {
					v.State, v.Error = "error", clipTo(err.Error(), maxErrorText)
				}
			}
		case !c.matches(r.vol):
			// UUID sama tetapi format, ukuran, atau nomor seri lain (drive
			// lain, misalnya hasil kloning): tidak dipasang otomatis.
			v = mergeVol(r.vol, v)
			v.State, v.Error = "error", "drive berbeda dari yang dulu dipasang (UUID sama); lepas lalu pasang lagi bila memang drive ini"
			if mounted {
				_ = e.umount(ctx, mp, false)
				changed = true
			}
		case !mounted:
			v = mergeVol(r.vol, v)
			if rs := e.retry[name]; now.Before(rs.next) {
				v.State, v.Error = "error", "pemasangan gagal; dicoba lagi nanti"
				break
			}
			if err := e.mountVol(ctx, name, c, r.path); err != nil {
				rs := e.retry[name]
				rs.delay = min(max(rs.delay*2, 10*time.Second), 10*time.Minute)
				rs.next = now.Add(rs.delay)
				e.retry[name] = rs
				v.State, v.Error = "error", clipTo(err.Error(), maxErrorText)
				e.log("drive eksternal gagal dipasang", "folder", name, "err", err)
			} else {
				delete(e.retry, name)
				v.State = "mounted"
				changed = true
				e.log("drive eksternal dipasang", "folder", name)
			}
		default:
			v = mergeVol(r.vol, v)
			v.State = "mounted"
		}
		if v.State == "mounted" {
			v.UsedBytes, v.AvailableBytes = statfsTimeout(mp, 2*time.Second)
		}
		out = append(out, v)
	}
	for _, r := range vols {
		if !seen[r.vol.UUID] || r.vol.UUID == "" {
			out = append(out, r.vol)
		}
	}
	e.cacheMu.Lock()
	e.cache = out
	e.cacheMu.Unlock()
	return out, changed, nil
}

// statfsTimeout: ruang terpakai dan kosong; drive yang tidak menjawab dalam
// batas waktu dilewati (0, 0).
func statfsTimeout(path string, d time.Duration) (used, avail uint64) {
	type res struct{ used, avail uint64 }
	ch := make(chan res, 1)
	go func() {
		var sfs syscall.Statfs_t
		if syscall.Statfs(path, &sfs) == nil {
			ch <- res{(sfs.Blocks - sfs.Bfree) * uint64(sfs.Bsize), sfs.Bavail * uint64(sfs.Bsize)}
		}
		close(ch)
	}()
	select {
	case r := <-ch:
		return r.used, r.avail
	case <-time.After(d):
		return 0, 0
	}
}

// mergeVol: info drive (r) dengan konfigurasi folder (c).
func mergeVol(r, c proto.ExternalVolume) proto.ExternalVolume {
	r.Name, r.ReadOnly, r.Group, r.SMB = c.Name, c.ReadOnly, c.Group, c.SMB
	r.Reason = ""
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

// checkExternalRoot: setelah dipasang, akar drive harus milik root dan grup
// itu tanpa akses untuk lainnya (metadata khusus di drive, misalnya atribut
// WSL di NTFS, tidak boleh membuka drive untuk semua akun).
func checkExternalRoot(path string, gid int) error {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return err
	}
	if st.Uid != 0 || int(st.Gid) != gid || st.Mode&0o007 != 0 {
		return errors.New("izin akar drive tidak sesuai opsi pasang; drive dilepas")
	}
	return nil
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
	if c.GID != 0 && gid != c.GID {
		return &OpError{Code: proto.ErrConflict, Message: "grup " + c.Group + " sudah berbeda dari saat drive dipasang; lepas lalu pasang lagi"}
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
	if err := e.rootOK(mp, gid); err != nil {
		_ = e.umount(ctx, mp, false)
		return err
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
	failing := false
	for {
		_, changed, err := e.refresh(ctx)
		switch {
		case err != nil && !failing:
			e.shares.p.logWarn("penyimpanan eksternal: drive tidak bisa diperiksa", "err", err)
			failing = true
		case err == nil:
			failing = false
			if changed {
				e.shares.refreshSMB(ctx)
			}
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
	e.cacheMu.RLock()
	defer e.cacheMu.RUnlock()
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
		gid, _ := e.gid(p.Group)
		c := externalConf{UUID: p.UUID, FSType: vol.vol.FSType, SizeBytes: vol.vol.SizeBytes, Serial: vol.vol.Serial,
			ReadOnly: p.ReadOnly, Group: p.Group, GID: gid, SMB: p.SMB}
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
