package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/proto"
)

// StorageTools adalah program dan kebijakan operasi storage.* (dari
// agent.storage di sapo.yaml).
type StorageTools struct {
	Lsblk          string
	Smartctl       string
	Timeout        time.Duration // batas waktu satu operasi, termasuk menunggu
	IgnoreDevices  []string
	MaxConcurrent  int           // program yang boleh berjalan bersamaan
	CacheFor       time.Duration // lama hasil SMART dan daftar disk dipakai ulang
	WarnATARawIDs  []int
	WarnNVMeFields []string
}

// Storage melayani operasi storage.*. Semua pemanggilan program berbagi satu
// batas jumlah bersamaan, dan SMART per disk di-cache: banyak tab panel atau
// tombol Perbarui yang ditekan berulang tidak menjalankan smartctl berkali-kali
// pada disk yang sama (yang bisa memperparah disk yang sedang bermasalah).
type Storage struct {
	t   StorageTools
	sem chan struct{}

	mu      sync.Mutex
	smart   map[string]*smartEntry
	disks   []proto.Disk
	disksAt time.Time
}

type smartEntry struct {
	done chan struct{} // ditutup setelah res terisi
	res  proto.SMART
	at   time.Time
}

// NewStorage menyiapkan operasi storage.*.
func NewStorage(t StorageTools) *Storage {
	if t.MaxConcurrent < 1 {
		t.MaxConcurrent = 1
	}
	return &Storage{t: t, sem: make(chan struct{}, t.MaxConcurrent), smart: map[string]*smartEntry{}}
}

// deviceNamePattern membatasi nama disk yang diterima dari panel. Nama juga
// harus ada di daftar storage.disks sebelum dipakai untuk membuka /dev.
var deviceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)

// Kolom lsblk yang dibaca (nama kolom util-linux).
var lsblkFlags = []string{
	"--json", "--bytes", "--tree",
	"--output", "NAME,PATH,TYPE,SIZE,MODEL,VENDOR,SERIAL,WWN,TRAN,ROTA,RM,FSTYPE,PTTYPE,LABEL,MOUNTPOINTS",
}

// Opsi smartctl: keluaran JSON ringkas, info perangkat, status kesehatan, dan
// atribut. "-n standby" membuat smartctl tidak membangunkan disk yang tidur.
var smartctlFlags = []string{"--json=c", "--info", "--health", "--attributes", "--nocheck=standby"}

// DisksOp mengembalikan handler storage.disks. Operasi ini hanya membaca.
func (st *Storage) DisksOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, st.t.Timeout)
		defer cancel()
		return st.listDisks(ctx)
	}
}

// SMARTOp mengembalikan handler storage.smart. Operasi ini hanya membaca.
func (st *Storage) SMARTOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.SMARTParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !deviceNamePattern.MatchString(p.Device) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama disk tidak valid"}
		}
		// Satu batas waktu untuk seluruh operasi (validasi + smartctl), di
		// bawah batas tunggu sapo-api.
		ctx, cancel := context.WithTimeout(ctx, st.t.Timeout)
		defer cancel()
		// Hanya disk yang benar-benar ada di daftar yang boleh dibaca.
		disks, err := st.cachedDisks(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range disks {
			if d.Name == p.Device {
				return st.smartOf(ctx, d.Name)
			}
		}
		return nil, &OpError{Code: proto.ErrNotFound, Message: "disk " + p.Device + " tidak ditemukan"}
	}
}

// run menjalankan program setelah mendapat giliran (semaphore).
func (st *Storage) run(ctx context.Context, prog string, flags []string, operands ...string) (toolResult, error) {
	select {
	case st.sem <- struct{}{}:
		defer func() { <-st.sem }()
	case <-ctx.Done():
		return toolResult{}, &OpError{Code: proto.ErrBusy, Message: "terlalu banyak permintaan storage bersamaan"}
	}
	timeout := st.t.Timeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	return runTool(ctx, timeout, prog, flags, operands...)
}

// cachedDisks memakai daftar disk terakhir bila masih segar (untuk validasi
// nama di storage.smart), selain itu membaca ulang.
func (st *Storage) cachedDisks(ctx context.Context) ([]proto.Disk, error) {
	st.mu.Lock()
	if st.disks != nil && time.Since(st.disksAt) < st.t.CacheFor {
		d := st.disks
		st.mu.Unlock()
		return d, nil
	}
	st.mu.Unlock()
	return st.listDisks(ctx)
}

// smartOf mengembalikan SMART disk dari cache, menunggu pembacaan yang sedang
// berjalan untuk disk yang sama, atau membaca baru.
func (st *Storage) smartOf(ctx context.Context, name string) (proto.SMART, error) {
	st.mu.Lock()
	e := st.smart[name]
	fresh := e != nil && (isOpen(e.done) || time.Since(e.at) < st.t.CacheFor)
	if !fresh {
		e = &smartEntry{done: make(chan struct{})}
		st.smart[name] = e
		st.mu.Unlock()
		e.res = st.readSMART(ctx, name)
		e.at = time.Now()
		if e.res.Status == proto.SMARTUnavailable {
			// Kegagalan sementara (sibuk, waktu habis) tidak di-cache.
			e.at = time.Time{}
		}
		close(e.done)
		return e.res, nil
	}
	st.mu.Unlock()
	select {
	case <-e.done:
		return e.res, nil
	case <-ctx.Done():
		return proto.SMART{}, &OpError{Code: proto.ErrBusy, Message: "pembacaan SMART " + name + " belum selesai"}
	}
}

func isOpen(c chan struct{}) bool {
	select {
	case <-c:
		return false
	default:
		return true
	}
}

func decodeParams(raw json.RawMessage, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return &OpError{Code: proto.ErrInvalidParams, Message: "parameter tidak valid"}
	}
	return nil
}

// forgetDisks membuang daftar disk yang di-cache (setelah disk diubah).
func (st *Storage) forgetDisks() {
	st.mu.Lock()
	st.disks = nil
	st.mu.Unlock()
}

func (st *Storage) listDisks(ctx context.Context) ([]proto.Disk, error) {
	res, err := st.run(ctx, st.t.Lsblk, lsblkFlags)
	if errors.Is(err, errToolMissing) {
		return nil, &OpError{Code: proto.ErrUnavailable, Message: err.Error()}
	}
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("lsblk keluar dengan kode %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	disks, err := parseLsblk(res.Stdout, st.t.IgnoreDevices)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	st.disks, st.disksAt = disks, time.Now()
	st.mu.Unlock()
	return disks, nil
}

// lsblkDevice mengikuti keluaran lsblk --json. Versi util-linux lama menulis
// angka dan boolean sebagai string, jadi kolom itu dibaca dengan flex*.
type lsblkDevice struct {
	Name        string        `json:"name"`
	Path        string        `json:"path"`
	Type        string        `json:"type"`
	Size        flexUint      `json:"size"`
	Model       *string       `json:"model"`
	Vendor      *string       `json:"vendor"`
	Serial      *string       `json:"serial"`
	WWN         *string       `json:"wwn"`
	Tran        *string       `json:"tran"`
	Rota        flexBool      `json:"rota"`
	RM          flexBool      `json:"rm"`
	FSType      *string       `json:"fstype"`
	PTType      *string       `json:"pttype"`
	Label       *string       `json:"label"`
	Mountpoints []*string     `json:"mountpoints"`
	Children    []lsblkDevice `json:"children"`
}

func parseLsblk(data []byte, ignore []string) ([]proto.Disk, error) {
	var out struct {
		BlockDevices []lsblkDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("keluaran lsblk tidak bisa dibaca: %w", err)
	}
	disks := make([]proto.Disk, 0, len(out.BlockDevices))
	for _, b := range out.BlockDevices {
		if b.Type != "disk" || !deviceNamePattern.MatchString(b.Name) || ignored(b.Name, ignore) {
			continue
		}
		d := proto.Disk{
			Name:       b.Name,
			Path:       b.Path,
			SizeBytes:  uint64(b.Size),
			Model:      str(b.Model),
			Vendor:     str(b.Vendor),
			Serial:     str(b.Serial),
			WWN:        str(b.WWN),
			Transport:  str(b.Tran),
			Rotational: bool(b.Rota),
			Removable:  bool(b.RM),
			FSType:     str(b.FSType),
			PTType:     str(b.PTType),
			Partitions: []proto.Partition{},
		}
		if d.Path == "" {
			d.Path = "/dev/" + d.Name
		}
		appendChildren(&d.Partitions, b.Children)
		d.Usage, d.UsageReason = diskUsage(b)
		disks = append(disks, d)
	}
	sort.Slice(disks, func(i, j int) bool { return naturalLess(disks[i].Name, disks[j].Name) })
	return disks, nil
}

// appendChildren meratakan pohon perangkat turunan (partisi, RAID, LVM, …).
func appendChildren(dst *[]proto.Partition, children []lsblkDevice) {
	for _, c := range children {
		p := proto.Partition{
			Name:        c.Name,
			SizeBytes:   uint64(c.Size),
			Type:        c.Type,
			FSType:      str(c.FSType),
			Label:       str(c.Label),
			Mountpoints: []string{},
		}
		for _, m := range c.Mountpoints {
			if m != nil && *m != "" {
				p.Mountpoints = append(p.Mountpoints, *m)
			}
		}
		*dst = append(*dst, p)
		appendChildren(dst, c.Children)
	}
}

// diskUsage menilai apakah disk boleh dipakai pool baru, dari disk itu dan
// seluruh turunannya. Urutan alasan penting: yang paling berbahaya bila
// dihapus (sistem berkas terpasang, swap) didahulukan.
func diskUsage(b lsblkDevice) (usage, reason string) {
	rank := map[string]int{proto.UsageMounted: 6, proto.UsageSwap: 5, proto.UsagePool: 4, proto.UsageRAID: 3, proto.UsageLVM: 2, proto.UsageCrypt: 1}
	hasData := str(b.FSType) != "" || str(b.PTType) != "" || len(b.Children) > 0
	var walk func(d lsblkDevice)
	walk = func(d lsblkDevice) {
		r := ""
		for _, m := range d.Mountpoints {
			switch {
			case m == nil || *m == "":
			case *m == "[SWAP]":
				r = pickReason(r, proto.UsageSwap, rank)
			default:
				r = pickReason(r, proto.UsageMounted, rank)
			}
		}
		switch fs := str(d.FSType); {
		case fs == "zfs_member":
			r = pickReason(r, proto.UsagePool, rank)
		case fs == "linux_raid_member" || strings.HasPrefix(d.Type, "raid"):
			r = pickReason(r, proto.UsageRAID, rank)
		case fs == "LVM2_member" || d.Type == "lvm":
			r = pickReason(r, proto.UsageLVM, rank)
		case d.Type == "crypt":
			r = pickReason(r, proto.UsageCrypt, rank)
		}
		reason = pickReason(reason, r, rank)
		for _, c := range d.Children {
			walk(c)
		}
	}
	walk(b)
	switch {
	case reason != "":
		return proto.DiskInUse, reason
	case hasData:
		return proto.DiskHasData, ""
	}
	return proto.DiskFree, ""
}

func pickReason(cur, next string, rank map[string]int) string {
	if rank[next] > rank[cur] {
		return next
	}
	return cur
}

func ignored(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// Bit kode keluar smartctl (lihat man smartctl, bagian RETURN VALUES).
const (
	smartExitParse      = 1 << 0 // opsi tidak bisa dibaca
	smartExitOpen       = 1 << 1 // perangkat gagal dibuka, atau sedang standby (-n)
	smartExitCmdFailed  = 1 << 2 // perintah SMART gagal atau checksum salah
	smartExitDiskFailed = 1 << 3 // status kesehatan: DISK FAILING
	smartExitPrefail    = 1 << 4 // atribut prefail <= ambang batas sekarang
	smartExitPastFail   = 1 << 5 // atribut pernah <= ambang batas
)

// Batas ukuran hasil SMART agar jawaban selalu muat di satu pesan protokol.
const (
	maxSMARTAttrs  = 256
	maxSMARTRawLen = 128
)

// smartctlOutput adalah bagian keluaran smartctl --json yang dipakai.
type smartctlOutput struct {
	Smartctl struct {
		ExitStatus int `json:"exit_status"`
		Messages   []struct {
			String   string `json:"string"`
			Severity string `json:"severity"`
		} `json:"messages"`
	} `json:"smartctl"`
	Device struct {
		Protocol string `json:"protocol"`
	} `json:"device"`
	SmartSupport *struct {
		Available bool `json:"available"`
		Enabled   bool `json:"enabled"`
	} `json:"smart_support"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current *int `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours *int `json:"hours"`
	} `json:"power_on_time"`
	ATAAttributes *struct {
		Table []struct {
			ID         int    `json:"id"`
			Name       string `json:"name"`
			Value      *int   `json:"value"`
			Worst      *int   `json:"worst"`
			Thresh     *int   `json:"thresh"`
			WhenFailed string `json:"when_failed"`
			Raw        struct {
				Value  uint64 `json:"value"`
				String string `json:"string"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	NVMeHealth map[string]json.RawMessage `json:"nvme_smart_health_information_log"`
}

func (st *Storage) readSMART(ctx context.Context, name string) proto.SMART {
	out := proto.SMART{Device: name, Attributes: []proto.SMARTAttr{}}
	res, err := st.run(ctx, st.t.Smartctl, smartctlFlags, "/dev/"+name)
	if err != nil {
		out.Status, out.Message = proto.SMARTUnavailable, err.Error()
		return out
	}
	return parseSMART(out, res, st.t)
}

func parseSMART(out proto.SMART, res toolResult, t StorageTools) proto.SMART {
	var s smartctlOutput
	if err := json.Unmarshal(res.Stdout, &s); err != nil {
		out.Status = proto.SMARTUnavailable
		out.Message = firstLine(res.Stderr)
		if out.Message == "" {
			out.Message = "keluaran smartctl tidak bisa dibaca"
		}
		return out
	}
	code := res.ExitCode
	msg := smartMessage(s)
	out.Protocol = s.Device.Protocol

	lower := strings.ToLower(msg)
	switch {
	// Disk virtual (misalnya virtio) tidak dikenali smartctl; kode keluarnya
	// 1, jadi diperiksa sebelum bit kode keluar.
	case strings.Contains(lower, "unable to detect device type"):
		out.Status, out.Message = proto.SMARTUnsupported, msg
		return out
	case code&smartExitParse != 0:
		out.Status, out.Message = proto.SMARTUnavailable, msg
		return out
	case code&smartExitOpen != 0:
		out.Status = proto.SMARTUnavailable
		if strings.Contains(lower, "standby") || strings.Contains(lower, "sleep") {
			out.Status = proto.SMARTStandby
		}
		out.Message = msg
		return out
	}

	if s.Temperature != nil {
		out.TemperatureC = s.Temperature.Current
	}
	if s.PowerOnTime != nil {
		out.PowerOnHours = s.PowerOnTime.Hours
	}
	warnIDs := map[int]bool{}
	for _, id := range t.WarnATARawIDs {
		warnIDs[id] = true
	}
	warnNVMe := map[string]bool{}
	for _, f := range t.WarnNVMeFields {
		warnNVMe[f] = true
	}
	anyFailing, anyWarning := false, false
	if s.ATAAttributes != nil {
		for _, a := range s.ATAAttributes.Table {
			attr := proto.SMARTAttr{
				ID: a.ID, Name: a.Name, Value: a.Value, Worst: a.Worst, Threshold: a.Thresh,
				Raw: clip(a.Raw.String), Failing: a.WhenFailed != "",
				Warning: warnIDs[a.ID] && a.Raw.Value > 0,
			}
			anyFailing = anyFailing || attr.Failing
			anyWarning = anyWarning || attr.Warning
			addAttr(&out, attr)
		}
	}
	keys := make([]string, 0, len(s.NVMeHealth))
	for k := range s.NVMeHealth {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		raw := strings.TrimSpace(string(s.NVMeHealth[k]))
		attr := proto.SMARTAttr{Name: k, Raw: clip(raw), Warning: warnNVMe[k] && raw != "0"}
		anyWarning = anyWarning || attr.Warning
		addAttr(&out, attr)
	}

	// Urutan penting (data dulu): rusak > peringatan > sehat. Firmware bisa
	// tetap melapor PASSED walau atribut prefail sudah melewati ambang batas
	// (bit 4/5), jadi PASSED saja tidak cukup untuk menyebut disk sehat.
	switch {
	case code&smartExitDiskFailed != 0 || (s.SmartStatus != nil && !s.SmartStatus.Passed):
		out.Status = proto.SMARTFailed
	case code&(smartExitPrefail|smartExitPastFail|smartExitCmdFailed) != 0 || anyFailing || anyWarning:
		out.Status = proto.SMARTWarning
	case s.SmartStatus != nil:
		out.Status = proto.SMARTPassed
	case s.SmartSupport != nil && !s.SmartSupport.Available:
		out.Status = proto.SMARTUnsupported
	default:
		out.Status = proto.SMARTUnavailable
	}
	if out.Status != proto.SMARTPassed {
		out.Message = msg
	}
	return out
}

func addAttr(out *proto.SMART, a proto.SMARTAttr) {
	if len(out.Attributes) < maxSMARTAttrs {
		out.Attributes = append(out.Attributes, a)
	}
}

func clip(s string) string { return clipTo(s, maxSMARTRawLen) }

// clipTo memotong s menjadi paling banyak n byte tanpa memotong karakter UTF-8.
func clipTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// smartMessage menggabungkan semua pesan smartctl, termasuk tingkat
// "information" (deteksi standby bergantung pada pesan itu).
func smartMessage(s smartctlOutput) string {
	var parts []string
	for _, m := range s.Smartctl.Messages {
		if m.String != "" {
			parts = append(parts, strings.TrimSpace(m.String))
		}
	}
	return strings.Join(parts, "; ")
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return line
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// naturalLess mengurutkan nama disk seperti yang diharapkan manusia:
// nvme2n1 < nvme10n1 dan sda < sdb < sdz < sdaa. Nama dipecah menjadi
// potongan huruf dan angka; angka dibandingkan nilainya. Potongan huruf yang
// awalannya sama (sdz, sdaa) dibandingkan panjangnya dulu, mengikuti
// penamaan sdX kernel; selain itu urutan abjad.
func naturalLess(a, b string) bool {
	ca, cb := chunks(a), chunks(b)
	for i := 0; i < len(ca) && i < len(cb); i++ {
		x, y := ca[i], cb[i]
		if x == y {
			continue
		}
		nx, errX := strconv.ParseUint(x, 10, 64)
		ny, errY := strconv.ParseUint(y, 10, 64)
		if errX == nil && errY == nil {
			return nx < ny
		}
		if len(x) != len(y) && len(x) > 2 && len(y) > 2 && x[:2] == y[:2] {
			return len(x) < len(y)
		}
		return x < y
	}
	return len(ca) < len(cb)
}

func chunks(s string) []string {
	var out []string
	start := 0
	for i := 1; i <= len(s); i++ {
		if i == len(s) || isDigit(s[i]) != isDigit(s[i-1]) {
			out = append(out, s[start:i])
			start = i
		}
	}
	return out
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// flexUint membaca angka JSON atau string berisi angka.
type flexUint uint64

func (f *flexUint) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return err
	}
	*f = flexUint(v)
	return nil
}

// flexBool membaca true/false JSON atau string "1"/"0".
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(b), `"`) {
	case "true", "1":
		*f = true
	default:
		*f = false
	}
	return nil
}
