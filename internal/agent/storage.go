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
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// StorageTools adalah program dan batas yang dipakai operasi storage.*
// (dari agent.storage di sapo.yaml).
type StorageTools struct {
	Lsblk         string
	Smartctl      string
	Timeout       time.Duration
	IgnoreDevices []string
}

// deviceNamePattern membatasi nama disk yang diterima dari panel. Nama juga
// harus ada di daftar storage.disks sebelum dipakai untuk membuka /dev.
var deviceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)

// Kolom lsblk yang dibaca (nama kolom util-linux).
var lsblkFlags = []string{
	"--json", "--bytes", "--tree",
	"--output", "NAME,PATH,TYPE,SIZE,MODEL,VENDOR,SERIAL,WWN,TRAN,ROTA,RM,FSTYPE,LABEL,MOUNTPOINTS",
}

// Opsi smartctl: keluaran JSON ringkas, info perangkat, status kesehatan, dan
// atribut. "-n standby" membuat smartctl tidak membangunkan disk yang tidur.
var smartctlFlags = []string{"--json=c", "--info", "--health", "--attributes", "--nocheck=standby"}

// StorageDisksOp mengembalikan handler storage.disks. Operasi ini hanya membaca.
func StorageDisksOp(t StorageTools) Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		return listDisks(ctx, t)
	}
}

// StorageSMARTOp mengembalikan handler storage.smart. Operasi ini hanya membaca.
func StorageSMARTOp(t StorageTools) Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.SMARTParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !deviceNamePattern.MatchString(p.Device) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama disk tidak valid"}
		}
		// Hanya disk yang benar-benar ada di daftar yang boleh dibaca.
		disks, err := listDisks(ctx, t)
		if err != nil {
			return nil, err
		}
		for _, d := range disks {
			if d.Name == p.Device {
				return readSMART(ctx, t, d.Name), nil
			}
		}
		return nil, &OpError{Code: proto.ErrNotFound, Message: "disk " + p.Device + " tidak ditemukan"}
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

func listDisks(ctx context.Context, t StorageTools) ([]proto.Disk, error) {
	res, err := runTool(ctx, t.Timeout, t.Lsblk, lsblkFlags)
	if errors.Is(err, errToolMissing) {
		return nil, &OpError{Code: proto.ErrUnavailable, Message: err.Error()}
	}
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("lsblk keluar dengan kode %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return parseLsblk(res.Stdout, t.IgnoreDevices)
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
			Partitions: []proto.Partition{},
		}
		if d.Path == "" {
			d.Path = "/dev/" + d.Name
		}
		appendChildren(&d.Partitions, b.Children)
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
	smartExitDiskFailed = 1 << 3 // status kesehatan: DISK FAILING
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
				String string `json:"string"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	NVMeHealth map[string]json.RawMessage `json:"nvme_smart_health_information_log"`
}

func readSMART(ctx context.Context, t StorageTools, name string) proto.SMART {
	out := proto.SMART{Device: name, Attributes: []proto.SMARTAttr{}}
	res, err := runTool(ctx, t.Timeout, t.Smartctl, smartctlFlags, "/dev/"+name)
	if err != nil {
		out.Status, out.Message = proto.SMARTUnavailable, err.Error()
		return out
	}
	return parseSMART(out, res)
}

func parseSMART(out proto.SMART, res toolResult) proto.SMART {
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

	switch {
	case code&smartExitDiskFailed != 0 || (s.SmartStatus != nil && !s.SmartStatus.Passed):
		out.Status = proto.SMARTFailed
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
	if s.Temperature != nil {
		out.TemperatureC = s.Temperature.Current
	}
	if s.PowerOnTime != nil {
		out.PowerOnHours = s.PowerOnTime.Hours
	}
	if s.ATAAttributes != nil {
		for _, a := range s.ATAAttributes.Table {
			out.Attributes = append(out.Attributes, proto.SMARTAttr{
				ID: a.ID, Name: a.Name, Value: a.Value, Worst: a.Worst, Threshold: a.Thresh,
				Raw: a.Raw.String, Failing: a.WhenFailed != "",
			})
		}
	}
	keys := make([]string, 0, len(s.NVMeHealth))
	for k := range s.NVMeHealth {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		raw := strings.TrimSpace(string(s.NVMeHealth[k]))
		out.Attributes = append(out.Attributes, proto.SMARTAttr{
			Name: k, Raw: raw, Failing: k == "critical_warning" && raw != "0",
		})
	}
	return out
}

// smartMessage menggabungkan pesan smartctl (tanpa pesan informasi biasa).
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
