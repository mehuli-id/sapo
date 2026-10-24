package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// PoolTools adalah program dan kebijakan operasi pool ZFS (dari
// agent.storage.pools di sapo.yaml).
type PoolTools struct {
	Zpool string
	// Pool dipasang di <MountBase>/<nama pool>.
	MountBase string
	Ashift    int
	// Properti dataset akar setiap pool baru (zpool create -O).
	RootProperties map[string]string
	Layouts        []proto.PoolLayout
	AllowRemovable bool
	CreateTimeout  time.Duration
	// ByIDDir adalah /dev/disk/by-id: pool dibuat dengan nama perangkat yang
	// tetap walau urutan sdX berubah setelah reboot.
	ByIDDir string
	// Log mencatat setiap perubahan pool (siapa pun pemanggilnya) di log sapod.
	Log *slog.Logger
}

// Pools melayani operasi storage.pools, storage.pool_create, dan
// storage.pool_scrub. Program dijalankan lewat Storage (batas jumlah
// bersamaan yang sama), dan hanya satu operasi yang mengubah pool berjalan
// pada satu waktu.
type Pools struct {
	st    *Storage
	t     PoolTools
	write sync.Mutex
	// openExcl memeriksa bahwa perangkat blok tidak sedang dipakai (diganti
	// di test; nil = tidak diperiksa).
	openExcl func(dev string) error
}

// NewPools menyiapkan operasi pool di atas st.
func NewPools(st *Storage, t PoolTools) *Pools {
	if t.ByIDDir == "" {
		t.ByIDDir = "/dev/disk/by-id"
	}
	p := &Pools{st: st, t: t, openExcl: openExclusive}
	st.poolMembers = p.memberDevices
	return p
}

// memberDevices mengembalikan nama perangkat kernel (disk atau partisi)
// anggota pool yang sedang aktif. ok=false bila tidak bisa dipastikan.
func (p *Pools) memberDevices(ctx context.Context) (map[string]bool, bool) {
	status, err := p.readStatus(ctx)
	if err != nil {
		return nil, false
	}
	out := map[string]bool{}
	var walk func(vs map[string]zpoolVdev)
	walk = func(vs map[string]zpoolVdev) {
		for _, v := range vs {
			if v.Path != "" {
				out[devName(v.Path)] = true
			}
			walk(v.Vdevs)
		}
	}
	for _, pool := range status {
		walk(pool.Vdevs)
	}
	return out, true
}

// readStatus membaca zpool status -j. Tanpa pool, hasilnya kosong.
func (p *Pools) readStatus(ctx context.Context) (map[string]zpoolStatusPool, error) {
	res, err := p.st.run(ctx, p.t.Zpool, []string{"status", "-j", "--json-int", "-p"})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("zpool status keluar dengan kode %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return parseZpoolStatus(res.Stdout)
}

// poolNamePattern membatasi nama pool baru: huruf kecil, angka, garis bawah,
// tanda hubung; diawali huruf. Lebih ketat dari aturan ZFS, agar nama juga
// aman sebagai nama folder dan di URL panel.
var poolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// validPoolName juga menolak nama yang dicadangkan ZFS (awalan mirror, raidz,
// draid, spare; nama log; dan c diikuti angka).
func validPoolName(name string) bool {
	if !poolNamePattern.MatchString(name) || name == "log" {
		return false
	}
	for _, p := range []string{"mirror", "raidz", "draid", "spare"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return !(len(name) > 1 && name[0] == 'c' && isDigit(name[1]))
}

// vdevKeyword adalah kata kunci vdev zpool create untuk setiap susunan.
var vdevKeyword = map[string]string{
	proto.LayoutSingle: "",
	proto.LayoutMirror: "mirror",
	proto.LayoutRAIDZ1: "raidz1",
	proto.LayoutRAIDZ2: "raidz2",
	proto.LayoutRAIDZ3: "raidz3",
}

// Batas ukuran hasil storage.pools agar selalu muat di satu pesan protokol.
const (
	maxPools     = 64
	maxVdevs     = 512
	maxErrorText = 512
)

// PoolsOp mengembalikan handler storage.pools. Operasi ini hanya membaca.
func (p *Pools) PoolsOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, p.st.t.Timeout)
		defer cancel()
		return p.listPools(ctx)
	}
}

func (p *Pools) listPools(ctx context.Context) (proto.PoolsResult, error) {
	out := proto.PoolsResult{Layouts: p.t.Layouts, AllowRemovable: p.t.AllowRemovable, Pools: []proto.Pool{}}
	if out.Layouts == nil {
		out.Layouts = []proto.PoolLayout{}
	}
	res, err := p.st.run(ctx, p.t.Zpool, []string{"list", "-H", "-p", "-o", "name,size,allocated,free,capacity,health"})
	if errors.Is(err, errToolMissing) {
		out.Message = "alat ZFS (zfsutils-linux) belum terpasang"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if res.ExitCode != 0 {
		// Misalnya modul kernel ZFS tidak termuat.
		out.Message = firstLine(res.Stderr)
		if out.Message == "" {
			out.Message = fmt.Sprintf("zpool keluar dengan kode %d", res.ExitCode)
		}
		return out, nil
	}
	out.Available = true
	out.Pools = parseZpoolList(res.Stdout)
	if len(out.Pools) == 0 {
		return out, nil
	}

	// Pohon vdev, scrub, dan pesan kesehatan. Bila gagal dibaca, daftar
	// pool dan kapasitasnya tetap ditampilkan.
	status, err := p.readStatus(ctx)
	if err != nil {
		p.logWarn("zpool status tidak terbaca", "err", err)
		return out, nil
	}
	disks, _ := p.st.cachedDisks(ctx)
	owner := partitionOwners(disks)
	budget := maxVdevs
	for i := range out.Pools {
		st, ok := status[out.Pools[i].Name]
		if !ok {
			continue
		}
		mergeStatus(&out.Pools[i], st, owner, &budget)
	}
	return out, nil
}

// parseZpoolList membaca zpool list -H -p (kolom dipisah tab, angka mentah;
// "-" untuk nilai yang tidak tersedia, misalnya pool yang rusak).
func parseZpoolList(data []byte) []proto.Pool {
	pools := []proto.Pool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() && len(pools) < maxPools {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 6 || f[0] == "" {
			continue
		}
		num := func(s string) uint64 { v, _ := strconv.ParseUint(s, 10, 64); return v }
		capPct, _ := strconv.Atoi(strings.TrimSuffix(f[4], "%"))
		pools = append(pools, proto.Pool{
			Name: f[0], SizeBytes: num(f[1]), AllocatedBytes: num(f[2]), FreeBytes: num(f[3]),
			CapacityPct: capPct, Health: f[5], Vdevs: []proto.Vdev{},
		})
	}
	sort.Slice(pools, func(i, j int) bool { return naturalLess(pools[i].Name, pools[j].Name) })
	return pools
}

// zpoolStatusJSON mengikuti zpool status -j --json-int (OpenZFS 2.3).
type zpoolStatusJSON struct {
	Pools map[string]zpoolStatusPool `json:"pools"`
}

type zpoolStatusPool struct {
	Name       string               `json:"name"`
	State      string               `json:"state"`
	Status     string               `json:"status"`
	Action     string               `json:"action"`
	ErrorCount flexUint             `json:"error_count"`
	ScanStats  *zpoolScanStats      `json:"scan_stats"`
	Vdevs      map[string]zpoolVdev `json:"vdevs"`
}

type zpoolVdev struct {
	Name           string               `json:"name"`
	GUID           flexUint             `json:"guid"`
	VdevType       string               `json:"vdev_type"`
	State          string               `json:"state"`
	Path           string               `json:"path"`
	ReadErrors     flexUint             `json:"read_errors"`
	WriteErrors    flexUint             `json:"write_errors"`
	ChecksumErrors flexUint             `json:"checksum_errors"`
	Vdevs          map[string]zpoolVdev `json:"vdevs"`
}

type zpoolScanStats struct {
	Function   string   `json:"function"`
	State      string   `json:"state"`
	StartTime  flexUint `json:"start_time"`
	EndTime    flexUint `json:"end_time"`
	ToExamine  flexUint `json:"to_examine"`
	Examined   flexUint `json:"examined"`
	Issued     flexUint `json:"issued"`
	Errors     flexUint `json:"errors"`
	ScrubPause flexUint `json:"scrub_pause"`
}

func parseZpoolStatus(data []byte) (map[string]zpoolStatusPool, error) {
	var s zpoolStatusJSON
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return s.Pools, nil
}

func mergeStatus(pool *proto.Pool, st zpoolStatusPool, owner map[string]string, budget *int) {
	pool.Status, pool.Action = clipTo(st.Status, maxErrorText), clipTo(st.Action, maxErrorText)
	pool.ErrorCount = uint64(st.ErrorCount)
	if sc := st.ScanStats; sc != nil && sc.Function != "" && sc.Function != "NONE" {
		pool.Scan = &proto.PoolScan{
			Function:  strings.ToLower(sc.Function),
			State:     strings.ToLower(sc.State),
			Paused:    sc.ScrubPause != 0,
			StartTime: int64(sc.StartTime),
			EndTime:   int64(sc.EndTime),
			Examined:  uint64(sc.Examined),
			ToExamine: uint64(sc.ToExamine),
			Issued:    uint64(sc.Issued),
			Errors:    uint64(sc.Errors),
		}
	}
	// Akar pohon adalah vdev bertipe root bernama sama dengan pool; yang
	// ditampilkan adalah anak-anaknya (vdev teratas).
	root, ok := st.Vdevs[pool.Name]
	if !ok {
		return
	}
	pool.Vdevs = convertVdevs(root.Vdevs, owner, budget)
	pool.Layout = layoutOf(pool.Vdevs)
}

func convertVdevs(in map[string]zpoolVdev, owner map[string]string, budget *int) []proto.Vdev {
	names := make([]string, 0, len(in))
	for k := range in {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	out := make([]proto.Vdev, 0, len(names))
	for _, k := range names {
		if *budget <= 0 {
			break
		}
		*budget--
		v := in[k]
		pv := proto.Vdev{
			Name: clip(v.Name), Type: v.VdevType, State: v.State, Path: clip(v.Path),
			ReadErrors: uint64(v.ReadErrors), WriteErrors: uint64(v.WriteErrors), ChecksumErrors: uint64(v.ChecksumErrors),
		}
		if pv.Name == "" {
			pv.Name = clip(k)
		}
		if v.GUID != 0 {
			pv.GUID = strconv.FormatUint(uint64(v.GUID), 10)
		}
		if v.Path != "" {
			pv.Disk = owner[devName(v.Path)]
		}
		if len(v.Vdevs) > 0 {
			pv.Children = convertVdevs(v.Vdevs, owner, budget)
		}
		out = append(out, pv)
	}
	return out
}

// layoutOf menyimpulkan susunan pool dari vdev teratasnya. Nama vdev raidz
// memuat paritasnya (raidz2-0).
func layoutOf(top []proto.Vdev) string {
	if len(top) == 0 {
		return ""
	}
	kind := func(v proto.Vdev) string {
		switch v.Type {
		case "mirror":
			return proto.LayoutMirror
		case "raidz":
			if p, _, ok := strings.Cut(v.Name, "-"); ok && vdevKeyword[p] != "" {
				return p
			}
			return proto.LayoutRAIDZ1
		case "disk", "file":
			return proto.LayoutSingle
		}
		return v.Type
	}
	first := kind(top[0])
	for _, v := range top[1:] {
		if kind(v) != first {
			return "mixed"
		}
	}
	if first == proto.LayoutSingle && len(top) > 1 {
		return "stripe"
	}
	return first
}

// devName mengubah path perangkat vdev (biasanya /dev/disk/by-id/…-part1)
// menjadi nama kernel (sdb1) dengan mengikuti symlink.
func devName(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return filepath.Base(path)
}

// partitionOwners memetakan nama disk dan partisinya ke nama disk.
func partitionOwners(disks []proto.Disk) map[string]string {
	out := map[string]string{}
	for _, d := range disks {
		out[d.Name] = d.Name
		for _, p := range d.Partitions {
			if _, taken := out[p.Name]; !taken {
				out[p.Name] = d.Name
			}
		}
	}
	return out
}

// PoolCreateOp mengembalikan handler storage.pool_create.
//
// Operasi ini menghapus isi disk yang dipilih, jadi sapod memeriksa ulang
// semuanya sendiri (tidak percaya pada pemeriksaan sapo-api atau panel):
// nama dan susunan pool, jumlah disk, identitas setiap disk (harus sama
// dengan yang dilihat pengguna), dan bahwa tidak ada disk yang sedang
// dipakai. Disk berisi data hanya boleh dipakai bila penghapusannya
// dikonfirmasi per disk.
func (p *Pools) PoolCreateOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.PoolCreateParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		layout, err := p.checkCreateParams(req)
		if err != nil {
			return nil, err
		}
		if !p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer p.write.Unlock()

		// Pemeriksaan memakai batas waktu biasa; zpool create mendapat batas
		// waktunya sendiri di bawah agar tidak terpotong waktu pemeriksaan.
		checkCtx, cancel := context.WithTimeout(ctx, p.st.t.Timeout)
		defer cancel()
		existing, err := p.poolNames(checkCtx)
		if err != nil {
			return nil, err
		}
		if existing[req.Name] {
			return nil, &OpError{Code: proto.ErrPoolExists, Message: "pool " + req.Name + " sudah ada"}
		}
		// Selalu dibaca baru (bukan dari cache): keadaan disk saat ini yang
		// menentukan.
		disks, err := p.st.listDisks(checkCtx)
		if err != nil {
			return nil, err
		}
		force, err := p.checkDisks(req.Disks, disks)
		if err != nil {
			return nil, err
		}
		// Lapisan terakhir: kernel sendiri yang memastikan tidak ada yang
		// sedang memegang disk atau partisinya (anggota btrfs multi-disk,
		// device-mapper, dan pemakai lain yang tidak terlihat di lsblk).
		if err := p.checkNotClaimed(req.Disks, disks); err != nil {
			return nil, err
		}

		devices := make([]string, 0, len(req.Disks))
		for _, d := range req.Disks {
			dev, err := p.stablePath(d)
			if err != nil {
				return nil, err
			}
			devices = append(devices, dev)
		}
		flags := []string{"create"}
		if force {
			// Ditimpa: tanda tangan lama di disk yang penghapusannya sudah
			// dikonfirmasi, atau ukuran disk yang berbeda di satu vdev.
			// Disk yang sedang dipakai sudah ditolak di atas.
			flags = append(flags, "-f")
		}
		flags = append(flags, "-o", "ashift="+strconv.Itoa(p.t.Ashift))
		for _, k := range sortedKeys(p.t.RootProperties) {
			flags = append(flags, "-O", k+"="+p.t.RootProperties[k])
		}
		flags = append(flags, "-m", filepath.Join(p.t.MountBase, req.Name))
		operands := []string{req.Name}
		if kw := vdevKeyword[layout.ID]; kw != "" {
			operands = append(operands, kw)
		}
		operands = append(operands, devices...)

		p.logInfo("membuat pool", "pool", req.Name, "susunan", layout.ID, "perangkat", devices, "paksa", force)
		// Tidak dibatalkan bila sapo-api memutus koneksi: zpool yang dihentikan
		// di tengah jalan bisa meninggalkan disk setengah berlabel.
		createCtx, cancelCreate := context.WithTimeout(context.WithoutCancel(ctx), p.t.CreateTimeout)
		defer cancelCreate()
		res, runErr := p.st.run(createCtx, p.t.Zpool, flags, operands...)
		p.st.forgetDisks()

		// Hasil dipastikan dari keadaan nyata: zpool bisa gagal melapor
		// (misalnya waktu habis) padahal pool sudah terbentuk.
		afterCtx, cancelAfter := context.WithTimeout(context.WithoutCancel(ctx), p.st.t.Timeout)
		defer cancelAfter()
		names, namesErr := p.poolNames(afterCtx)
		created := namesErr == nil && names[req.Name]
		if !created {
			msg := ""
			switch {
			case runErr != nil:
				msg = runErr.Error()
			case res.ExitCode != 0:
				msg = clipTo(strings.TrimSpace(string(res.Stderr)), maxErrorText)
			case namesErr != nil:
				msg = "pool mungkin sudah dibuat, tetapi daftar pool tidak terbaca: " + namesErr.Error()
			default:
				msg = "zpool selesai tanpa error, tetapi pool tidak ditemukan"
			}
			p.logWarn("pool gagal dibuat", "pool", req.Name, "kode", res.ExitCode, "pesan", msg)
			return nil, &OpError{Code: "pool_create_failed", Message: msg}
		}
		if runErr != nil || res.ExitCode != 0 {
			p.logWarn("zpool create melapor gagal, tetapi pool terbentuk", "pool", req.Name, "err", runErr, "kode", res.ExitCode)
		}
		p.logInfo("pool dibuat", "pool", req.Name)
		list, err := p.listPools(afterCtx)
		if err != nil {
			// Pool sudah ada; detailnya tampil saat panel memuat ulang.
			return proto.Pool{Name: req.Name, Vdevs: []proto.Vdev{}}, nil
		}
		for _, pool := range list.Pools {
			if pool.Name == req.Name {
				return pool, nil
			}
		}
		return proto.Pool{Name: req.Name, Vdevs: []proto.Vdev{}}, nil
	}
}

// checkCreateParams memeriksa bentuk permintaan tanpa menjalankan apa pun.
func (p *Pools) checkCreateParams(req proto.PoolCreateParams) (proto.PoolLayout, error) {
	invalid := func(msg string) (proto.PoolLayout, error) {
		return proto.PoolLayout{}, &OpError{Code: proto.ErrInvalidParams, Message: msg}
	}
	if !validPoolName(req.Name) {
		return invalid("nama pool tidak valid")
	}
	var layout proto.PoolLayout
	for _, l := range p.t.Layouts {
		if l.ID == req.Layout {
			layout = l
		}
	}
	if layout.ID == "" {
		return invalid("susunan pool tidak tersedia")
	}
	if n := len(req.Disks); n < layout.MinDisks || n > layout.MaxDisks {
		return invalid(fmt.Sprintf("susunan %s butuh %d sampai %d disk", layout.ID, layout.MinDisks, layout.MaxDisks))
	}
	seen := map[string]bool{}
	for _, d := range req.Disks {
		if !deviceNamePattern.MatchString(d.Name) {
			return invalid("nama disk tidak valid")
		}
		if seen[d.Name] {
			return invalid("disk " + d.Name + " dipilih dua kali")
		}
		seen[d.Name] = true
	}
	return layout, nil
}

// checkDisks membandingkan disk yang dipilih dengan keadaan sekarang. force
// bernilai true bila zpool create perlu -f.
func (p *Pools) checkDisks(refs []proto.DiskRef, disks []proto.Disk) (force bool, err error) {
	byName := map[string]proto.Disk{}
	for _, d := range disks {
		byName[d.Name] = d
	}
	var size uint64
	for i, ref := range refs {
		d, ok := byName[ref.Name]
		if !ok {
			return false, &OpError{Code: proto.ErrDiskChanged, Message: "disk " + ref.Name + " tidak ditemukan lagi"}
		}
		// Status pakai juga bagian dari identitas: disk kosong yang tiba-tiba
		// berisi data (dipartisi ulang, atau ditukar dengan disk lain
		// berukuran sama tanpa nomor seri) tidak boleh ikut terhapus.
		if d.Serial != ref.Serial || d.WWN != ref.WWN || d.SizeBytes != ref.SizeBytes || d.Usage != ref.Usage {
			return false, &OpError{Code: proto.ErrDiskChanged, Message: "disk " + ref.Name + " sudah berbeda dari yang dipilih"}
		}
		switch {
		case d.Usage == proto.DiskInUse:
			return false, &OpError{Code: proto.ErrDiskInUse, Message: "disk " + ref.Name + " sedang dipakai (" + d.UsageReason + ")"}
		case d.Usage != proto.DiskFree && d.Usage != proto.DiskHasData:
			return false, &OpError{Code: proto.ErrDiskInUse, Message: "status disk " + ref.Name + " tidak dikenal"}
		case d.Removable && !p.t.AllowRemovable:
			return false, &OpError{Code: proto.ErrDiskRemovable, Message: "disk " + ref.Name + " adalah disk lepas-pasang"}
		case d.Usage == proto.DiskHasData && !ref.Erase:
			return false, &OpError{Code: proto.ErrDiskHasData, Message: "disk " + ref.Name + " berisi data; penghapusan belum dikonfirmasi"}
		}
		if d.Usage == proto.DiskHasData {
			force = true
		}
		if i > 0 && d.SizeBytes != size {
			force = true
		}
		size = d.SizeBytes
	}
	return force, nil
}

// poolNames membaca nama pool yang sudah ada (termasuk yang sedang rusak).
func (p *Pools) poolNames(ctx context.Context) (map[string]bool, error) {
	res, err := p.st.run(ctx, p.t.Zpool, []string{"list", "-H", "-o", "name"})
	if errors.Is(err, errToolMissing) {
		return nil, &OpError{Code: proto.ErrUnavailable, Message: "alat ZFS (zfsutils-linux) belum terpasang"}
	}
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, &OpError{Code: proto.ErrUnavailable, Message: firstLine(res.Stderr)}
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.ContainsRune(line, ' ') {
			names[line] = true
		}
	}
	return names, nil
}

// stablePath memilih nama perangkat yang tidak berubah antar-boot dari
// /dev/disk/by-id (WWN dulu, lalu nomor seri). Nama itu harus memuat WWN atau
// nomor seri yang sudah diperiksa, dan harus masih menunjuk ke disk yang sama.
// Bila disk tidak punya keduanya (misalnya disk virtual), dipakai /dev/<nama>;
// ZFS tetap menemukan disknya saat impor karena label pool tersimpan di disk.
func (p *Pools) stablePath(ref proto.DiskRef) (string, error) {
	name := ref.Name
	fallback := func() (string, error) {
		if ref.Serial != "" || ref.WWN != "" {
			// Disk beridentitas harus muncul di by-id; bila tidak, ada yang aneh.
			return "", &OpError{Code: proto.ErrDiskChanged, Message: "nama tetap disk " + name + " tidak ditemukan"}
		}
		return "/dev/" + name, nil
	}
	entries, err := os.ReadDir(p.t.ByIDDir)
	if err != nil {
		return fallback()
	}
	best, bestRank := "", len(byIDPrefixes)+1
	for _, e := range entries {
		n := e.Name()
		if partSuffix.MatchString(n) || !matchesIdentity(n, ref) {
			continue
		}
		target, err := filepath.EvalSymlinks(filepath.Join(p.t.ByIDDir, n))
		if err != nil || filepath.Base(target) != name {
			continue
		}
		rank := len(byIDPrefixes)
		for i, prefix := range byIDPrefixes {
			if strings.HasPrefix(n, prefix) {
				rank = i
				break
			}
		}
		if rank < bestRank || (rank == bestRank && n < best) {
			best, bestRank = n, rank
		}
	}
	if best == "" {
		return fallback()
	}
	return filepath.Join(p.t.ByIDDir, best), nil
}

// matchesIdentity: nama by-id memuat WWN (wwn-0x…, nvme-eui.…) atau nomor
// seri disk yang sudah diperiksa.
func matchesIdentity(link string, ref proto.DiskRef) bool {
	wwn := strings.TrimPrefix(strings.ToLower(ref.WWN), "0x")
	switch {
	case wwn != "" && strings.Contains(strings.ToLower(link), wwn):
		return true
	case ref.Serial != "" && strings.Contains(link, strings.ReplaceAll(ref.Serial, " ", "_")):
		return true
	}
	return false
}

// checkNotClaimed membuka setiap disk dan partisinya dengan O_EXCL. Kernel
// menolak (EBUSY) perangkat blok yang sedang dipakai: terpasang, swap,
// anggota RAID, device-mapper, atau btrfs multi-disk.
func (p *Pools) checkNotClaimed(refs []proto.DiskRef, disks []proto.Disk) error {
	if p.openExcl == nil {
		return nil
	}
	byName := map[string]proto.Disk{}
	for _, d := range disks {
		byName[d.Name] = d
	}
	for _, ref := range refs {
		d := byName[ref.Name]
		names := []string{d.Name}
		for _, part := range d.Partitions {
			if part.Type == "part" {
				names = append(names, part.Name)
			}
		}
		for _, n := range names {
			if err := p.openExcl("/dev/" + n); err != nil {
				return &OpError{Code: proto.ErrDiskInUse, Message: "disk " + ref.Name + " sedang dipakai sistem (" + n + ": " + err.Error() + ")"}
			}
		}
	}
	return nil
}

// byIDPrefixes: urutan pilihan nama di /dev/disk/by-id, dari yang paling
// tetap (WWN, tertanam di disk) sampai yang bergantung pada adaptor (usb).
var byIDPrefixes = []string{"wwn-", "nvme-eui.", "ata-", "nvme-", "scsi-", "virtio-", "usb-"}

var partSuffix = regexp.MustCompile(`-part[0-9]+$`)

// PoolScrubOp mengembalikan handler storage.pool_scrub. Scrub hanya membaca
// dan memperbaiki data dari salinan yang sehat; tidak menghapus apa pun.
func (p *Pools) PoolScrubOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.PoolScrubParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		if !poolNamePattern.MatchString(req.Name) || (req.Action != "start" && req.Action != "stop") {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "parameter scrub tidak valid"}
		}
		if !p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer p.write.Unlock()
		ctx, cancel := context.WithTimeout(ctx, p.st.t.Timeout)
		defer cancel()
		names, err := p.poolNames(ctx)
		if err != nil {
			return nil, err
		}
		if !names[req.Name] {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "pool " + req.Name + " tidak ditemukan"}
		}
		flags := []string{"scrub"}
		if req.Action == "stop" {
			flags = append(flags, "-s")
		}
		res, err := p.st.run(ctx, p.t.Zpool, flags, req.Name)
		if err != nil {
			return nil, err
		}
		msg := strings.ToLower(string(res.Stderr))
		switch {
		case res.ExitCode == 0,
			// Sudah dalam keadaan yang diminta: dianggap berhasil.
			req.Action == "start" && strings.Contains(msg, "currently scrubbing"),
			req.Action == "stop" && strings.Contains(msg, "no active scrub"):
		case strings.Contains(msg, "currently resilvering"):
			return nil, &OpError{Code: proto.ErrPoolBusy, Message: "pool " + req.Name + " sedang resilver"}
		default:
			return nil, &OpError{Code: "pool_scrub_failed", Message: clipTo(strings.TrimSpace(string(res.Stderr)), maxErrorText)}
		}
		p.logInfo("scrub", "pool", req.Name, "aksi", req.Action)
		return map[string]bool{"ok": true}, nil
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (p *Pools) logInfo(msg string, args ...any) {
	if p.t.Log != nil {
		p.t.Log.Info(msg, args...)
	}
}

func (p *Pools) logWarn(msg string, args ...any) {
	if p.t.Log != nil {
		p.t.Log.Warn(msg, args...)
	}
}

var guidPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

// PoolReplaceOp mengembalikan handler storage.pool_replace: disk lama
// (ditunjuk dengan GUID ZFS, juga bila disknya hilang) diganti disk baru,
// lalu ZFS membangun ulang datanya (resilver). Disk baru diperiksa sama
// ketatnya dengan disk untuk pool baru, karena isinya dihapus.
func (p *Pools) PoolReplaceOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.PoolReplaceParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		if !poolNamePattern.MatchString(req.Name) || !guidPattern.MatchString(req.OldGUID) || !deviceNamePattern.MatchString(req.Disk.Name) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "parameter penggantian disk tidak valid"}
		}
		if !p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer p.write.Unlock()

		checkCtx, cancel := context.WithTimeout(ctx, p.st.t.Timeout)
		defer cancel()
		status, err := p.readStatus(checkCtx)
		if err != nil {
			return nil, &OpError{Code: proto.ErrUnavailable, Message: err.Error()}
		}
		pool, ok := status[req.Name]
		if !ok {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "pool " + req.Name + " tidak ditemukan"}
		}
		if !hasLeaf(pool.Vdevs, req.OldGUID) {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "disk yang diganti tidak ada di pool " + req.Name}
		}
		disks, err := p.st.listDisks(checkCtx)
		if err != nil {
			return nil, err
		}
		force, err := p.checkDisks([]proto.DiskRef{req.Disk}, disks)
		if err != nil {
			return nil, err
		}
		if err := p.checkNotClaimed([]proto.DiskRef{req.Disk}, disks); err != nil {
			return nil, err
		}
		dev, err := p.stablePath(req.Disk)
		if err != nil {
			return nil, err
		}

		flags := []string{"replace"}
		if force {
			flags = append(flags, "-f")
		}
		flags = append(flags, "-o", "ashift="+strconv.Itoa(p.t.Ashift))
		p.logInfo("mengganti disk pool", "pool", req.Name, "lama", req.OldGUID, "baru", dev, "paksa", force)
		runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(ctx), p.t.CreateTimeout)
		defer cancelRun()
		res, err := p.st.run(runCtx, p.t.Zpool, flags, req.Name, req.OldGUID, dev)
		p.st.forgetDisks()
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			msg := clipTo(strings.TrimSpace(string(res.Stderr)), maxErrorText)
			p.logWarn("disk gagal diganti", "pool", req.Name, "kode", res.ExitCode, "stderr", msg)
			return nil, &OpError{Code: "pool_replace_failed", Message: msg}
		}
		p.logInfo("disk diganti, resilver dimulai", "pool", req.Name)
		return map[string]bool{"ok": true}, nil
	}
}

// hasLeaf: guid adalah disk (bukan grup mirror/raidz) di pohon vdev.
func hasLeaf(vs map[string]zpoolVdev, guid string) bool {
	for _, v := range vs {
		if len(v.Vdevs) == 0 && v.VdevType != "root" && strconv.FormatUint(uint64(v.GUID), 10) == guid {
			return true
		}
		if hasLeaf(v.Vdevs, guid) {
			return true
		}
	}
	return false
}

// PoolDestroyOp mengembalikan handler storage.pool_destroy. Tanpa -f: pool
// yang sedang dipakai (misalnya folder yang sedang dibuka lewat jaringan)
// tidak dihapus. Label ZFS di disk dibiarkan, jadi pool yang terhapus tidak
// sengaja masih bisa dipulihkan (zpool import -D) selama disknya belum
// dipakai lagi; disk itu tampil sebagai "berisi data".
func (p *Pools) PoolDestroyOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.PoolDestroyParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		if !poolNamePattern.MatchString(req.Name) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama pool tidak valid"}
		}
		if !p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer p.write.Unlock()
		checkCtx, cancel := context.WithTimeout(ctx, p.st.t.Timeout)
		defer cancel()
		names, err := p.poolNames(checkCtx)
		if err != nil {
			return nil, err
		}
		if !names[req.Name] {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "pool " + req.Name + " tidak ditemukan"}
		}
		p.logInfo("menghapus pool", "pool", req.Name)
		runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(ctx), p.t.CreateTimeout)
		defer cancelRun()
		res, err := p.st.run(runCtx, p.t.Zpool, []string{"destroy"}, req.Name)
		p.st.forgetDisks()
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			msg := clipTo(strings.TrimSpace(string(res.Stderr)), maxErrorText)
			p.logWarn("pool gagal dihapus", "pool", req.Name, "kode", res.ExitCode, "stderr", msg)
			code := "pool_destroy_failed"
			if strings.Contains(strings.ToLower(msg), "busy") {
				code = proto.ErrPoolBusy
			}
			return nil, &OpError{Code: code, Message: msg}
		}
		p.logInfo("pool dihapus", "pool", req.Name)
		return map[string]bool{"ok": true}, nil
	}
}
