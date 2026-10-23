package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// fakeTool menulis skrip pengganti program sistem: argumennya dicatat ke
// <dir>/<nama>.args (satu per baris), lalu skrip mencetak fixture dan keluar
// dengan kode exit. Tidak ada disk sungguhan yang disentuh test.
func fakeTool(t *testing.T, dir, name, fixture string, exit int) string {
	t.Helper()
	prog := filepath.Join(dir, name)
	args := prog + ".args"
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + args + "'\n"
	if fixture != "" {
		abs, err := filepath.Abs(filepath.Join("testdata", "storage", fixture))
		if err != nil {
			t.Fatal(err)
		}
		script += "cat '" + abs + "'\n"
	}
	script += "exit " + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return prog
}

func toolArgs(t *testing.T, prog string) []string {
	t.Helper()
	b, err := os.ReadFile(prog + ".args")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func storageTools(t *testing.T, smartFixture string, smartExit int) (StorageTools, string, string) {
	t.Helper()
	dir := t.TempDir()
	lsblk := fakeTool(t, dir, "lsblk", "lsblk.json", 0)
	smartctl := fakeTool(t, dir, "smartctl", smartFixture, smartExit)
	return StorageTools{
		Lsblk: lsblk, Smartctl: smartctl, Timeout: 5 * time.Second, IgnoreDevices: []string{"zram*", "ram*", "loop*"},
		MaxConcurrent: 2, WarnATARawIDs: []int{5, 187, 197, 198}, WarnNVMeFields: []string{"media_errors", "critical_warning"},
	}, lsblk, smartctl
}

func TestStorageDisksListsOnlyRealDisks(t *testing.T) {
	tools, lsblk, _ := storageTools(t, "", 0)
	res, err := NewStorage(tools).DisksOp()(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	disks := res.([]proto.Disk)
	var names []string
	for _, d := range disks {
		names = append(names, d.Name)
	}
	// zram (diabaikan), loop, dan CD-ROM bukan disk; urutan alami.
	if want := []string{"nvme2n1", "nvme10n1", "sda", "sdb"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("disk = %v, harus %v", names, want)
	}
	sdb := disks[3]
	if sdb.Vendor != "ATA" || sdb.Serial != "WD-WCC7K1234567" || !sdb.Rotational || sdb.Transport != "sata" || sdb.SizeBytes != 4000787030016 {
		t.Fatalf("sdb = %+v", sdb)
	}
	// Pohon partisi diratakan, termasuk RAID di atas partisi.
	if len(sdb.Partitions) != 2 || sdb.Partitions[1].Name != "md0" || sdb.Partitions[1].Type != "raid1" ||
		!reflect.DeepEqual(sdb.Partitions[1].Mountpoints, []string{"/srv/data"}) {
		t.Fatalf("partisi sdb = %+v", sdb.Partitions)
	}
	// Mountpoint null dari lsblk tidak menjadi string kosong.
	if len(sdb.Partitions[0].Mountpoints) != 0 {
		t.Fatalf("mountpoint sdb1 = %q", sdb.Partitions[0].Mountpoints)
	}
	args := toolArgs(t, lsblk)
	if args[len(args)-1] != "--" {
		t.Fatalf("argumen lsblk harus diakhiri --, dapat %q", args)
	}
}

func TestParseLsblkOldStringFormat(t *testing.T) {
	// util-linux lama menulis angka dan boolean sebagai string.
	data := []byte(`{"blockdevices":[{"name":"sda","type":"disk","size":"1000","rota":"1","rm":"0","mountpoints":[null]}]}`)
	disks, err := parseLsblk(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 || disks[0].SizeBytes != 1000 || !disks[0].Rotational || disks[0].Removable || disks[0].Path != "/dev/sda" {
		t.Fatalf("disk = %+v", disks)
	}
}

func TestParseLsblkSkipsUnsafeNames(t *testing.T) {
	data := []byte(`{"blockdevices":[{"name":"../etc","type":"disk","size":1},{"name":"sd a","type":"disk","size":1},{"name":"sdc","type":"disk","size":1}]}`)
	disks, err := parseLsblk(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 || disks[0].Name != "sdc" {
		t.Fatalf("disk = %+v", disks)
	}
}

func smartCall(t *testing.T, tools StorageTools, params string) (proto.SMART, error) {
	t.Helper()
	res, err := NewStorage(tools).SMARTOp()(context.Background(), json.RawMessage(params))
	if err != nil {
		return proto.SMART{}, err
	}
	return res.(proto.SMART), nil
}

func TestStorageSMARTPassedATA(t *testing.T) {
	tools, _, smartctl := storageTools(t, "smart-ata-passed.json", 0)
	s, err := smartCall(t, tools, `{"device":"sdb"}`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != proto.SMARTPassed || s.Protocol != "ATA" || s.Message != "" {
		t.Fatalf("smart = %+v", s)
	}
	if s.TemperatureC == nil || *s.TemperatureC != 34 || s.PowerOnHours == nil || *s.PowerOnHours != 28734 {
		t.Fatalf("suhu/jam = %v %v", s.TemperatureC, s.PowerOnHours)
	}
	if len(s.Attributes) != 4 || s.Attributes[1].ID != 5 || *s.Attributes[1].Threshold != 140 || s.Attributes[1].Failing {
		t.Fatalf("atribut = %+v", s.Attributes)
	}
	// Perangkat selalu sesudah "--"; disk yang tidur tidak dibangunkan.
	args := toolArgs(t, smartctl)
	if n := len(args); n < 2 || args[n-2] != "--" || args[n-1] != "/dev/sdb" {
		t.Fatalf("argumen smartctl = %q", args)
	}
	if !contains(args, "--nocheck=standby") {
		t.Fatalf("smartctl harus memakai --nocheck=standby: %q", args)
	}
}

func TestStorageSMARTStatuses(t *testing.T) {
	cases := []struct {
		fixture string
		exit    int
		device  string
		status  string
	}{
		{"smart-ata-failing.json", 24, "sdb", proto.SMARTFailed},
		// Firmware masih PASSED, tetapi atribut prefail melewati ambang batas.
		{"smart-ata-prefail.json", 16, "sdb", proto.SMARTWarning},
		// PASSED tanpa bit apa pun, tetapi sektor dipindahkan (atribut 5) > 0.
		{"smart-ata-reallocated.json", 0, "sdb", proto.SMARTWarning},
		{"smart-nvme-media-errors.json", 0, "nvme2n1", proto.SMARTWarning},
		{"smart-standby.json", 2, "sdb", proto.SMARTStandby},
		{"smart-permission.json", 2, "sdb", proto.SMARTUnavailable},
		{"smart-virtio.json", 1, "sda", proto.SMARTUnsupported},
		{"", 2, "sda", proto.SMARTUnavailable}, // bukan JSON sama sekali
	}
	for _, c := range cases {
		tools, _, _ := storageTools(t, c.fixture, c.exit)
		s, err := smartCall(t, tools, `{"device":"`+c.device+`"}`)
		if err != nil {
			t.Fatalf("%s: %v", c.fixture, err)
		}
		if s.Status != c.status {
			t.Errorf("%s: status = %q, harus %q (%s)", c.fixture, s.Status, c.status, s.Message)
		}
		if c.status != proto.SMARTPassed && c.status != proto.SMARTWarning && s.Message == "" {
			t.Errorf("%s: status %s harus disertai pesan", c.fixture, s.Status)
		}
	}
}

func TestStorageSMARTFailingAttribute(t *testing.T) {
	tools, _, _ := storageTools(t, "smart-ata-failing.json", 24)
	s, err := smartCall(t, tools, `{"device":"sdb"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Attributes) != 1 || !s.Attributes[0].Failing || s.Attributes[0].Raw != "1832" {
		t.Fatalf("atribut = %+v", s.Attributes)
	}
}

func TestStorageSMARTNVMe(t *testing.T) {
	tools, _, _ := storageTools(t, "smart-nvme.json", 0)
	s, err := smartCall(t, tools, `{"device":"nvme2n1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != proto.SMARTPassed || s.Protocol != "NVMe" || *s.TemperatureC != 38 {
		t.Fatalf("smart = %+v", s)
	}
	names := make([]string, 0, len(s.Attributes))
	for _, a := range s.Attributes {
		names = append(names, a.Name)
		if a.Failing {
			t.Fatalf("%s tidak boleh failing", a.Name)
		}
	}
	if !sort.StringsAreSorted(names) || !contains(names, "media_errors") {
		t.Fatalf("nama atribut NVMe = %v", names)
	}
}

func TestStorageSMARTRejectsBadDevice(t *testing.T) {
	tools, _, smartctl := storageTools(t, "smart-ata-passed.json", 0)
	for _, p := range []string{`{"device":"../../etc/passwd"}`, `{"device":"-a"}`, `{"device":"sda1"}`, `{"device":"zram0"}`, `{"device":"sr0"}`, `{"device":""}`, `{"device":"sda","x":1}`, `not json`} {
		_, err := smartCall(t, tools, p)
		var oe *OpError
		if !errors.As(err, &oe) || (oe.Code != proto.ErrInvalidParams && oe.Code != proto.ErrNotFound) {
			t.Errorf("%s: harus ditolak, dapat %v", p, err)
		}
	}
	// smartctl tidak pernah dijalankan untuk nama yang ditolak.
	if _, err := os.Stat(smartctl + ".args"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("smartctl tidak boleh dijalankan untuk perangkat yang ditolak")
	}
}

func TestStorageUnavailableWithoutLsblk(t *testing.T) {
	tools := StorageTools{Lsblk: filepath.Join(t.TempDir(), "tidak-ada"), Smartctl: "/bin/false", Timeout: time.Second}
	_, err := NewStorage(tools).DisksOp()(context.Background(), nil)
	var oe *OpError
	if !errors.As(err, &oe) || oe.Code != proto.ErrUnavailable {
		t.Fatalf("harus unavailable, dapat %v", err)
	}
}

func TestRunToolLimits(t *testing.T) {
	dir := t.TempDir()
	if _, err := runTool(context.Background(), time.Second, "sh", nil); !errors.Is(err, errToolMissing) {
		t.Fatalf("path relatif harus ditolak, dapat %v", err)
	}
	slow := filepath.Join(dir, "slow")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := runTool(context.Background(), 200*time.Millisecond, slow, nil); err == nil || !strings.Contains(err.Error(), "batas waktu") {
		t.Fatalf("harus habis waktu, dapat %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("program yang lambat harus dihentikan")
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte("#!/bin/sh\nhead -c 5000000 /dev/zero\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(context.Background(), 5*time.Second, big, nil); err == nil || !strings.Contains(err.Error(), "melebihi") {
		t.Fatalf("keluaran besar harus ditolak, dapat %v", err)
	}
	env := filepath.Join(dir, "env")
	if err := os.WriteFile(env, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAPO_RAHASIA", "jangan-bocor")
	res, err := runTool(context.Background(), 5*time.Second, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Stdout), "SAPO_RAHASIA") || !strings.Contains(string(res.Stdout), "LC_ALL=C") {
		t.Fatalf("lingkungan program anak harus tetap dan minimal:\n%s", res.Stdout)
	}
}

func TestNaturalLess(t *testing.T) {
	got := []string{"sdaa", "nvme10n1", "sdb", "sda", "nvme2n1", "sdz", "vda"}
	sort.Slice(got, func(i, j int) bool { return naturalLess(got[i], got[j]) })
	if want := []string{"nvme2n1", "nvme10n1", "sda", "sdb", "sdz", "sdaa", "vda"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("urutan = %v, harus %v", got, want)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestStorageSMARTCacheAndConcurrency(t *testing.T) {
	dir := t.TempDir()
	lsblk := fakeTool(t, dir, "lsblk", "lsblk.json", 0)
	// smartctl palsu yang lambat dan menghitung berapa kali dijalankan.
	count := filepath.Join(dir, "count")
	fixture, _ := filepath.Abs(filepath.Join("testdata", "storage", "smart-ata-passed.json"))
	smartctl := filepath.Join(dir, "smartctl")
	script := "#!/bin/sh\necho x >> '" + count + "'\nsleep 0.3\ncat '" + fixture + "'\n"
	if err := os.WriteFile(smartctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	st := NewStorage(StorageTools{Lsblk: lsblk, Smartctl: smartctl, Timeout: 5 * time.Second, MaxConcurrent: 1, CacheFor: time.Minute})
	op := st.SMARTOp()

	// Sepuluh permintaan bersamaan untuk disk yang sama = satu smartctl.
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			res, err := op(context.Background(), json.RawMessage(`{"device":"sdb"}`))
			if err == nil && res.(proto.SMART).Status != proto.SMARTPassed {
				err = errors.New("status salah")
			}
			errs <- err
		}()
	}
	for i := 0; i < 10; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(count); strings.Count(string(b), "x") != 1 {
		t.Fatalf("smartctl dijalankan %d kali, harus 1", strings.Count(string(b), "x"))
	}

	// Permintaan yang menunggu giliran terlalu lama dijawab sibuk, bukan menggantung.
	st.sem <- struct{}{}
	defer func() { <-st.sem }()
	_, err := storageWithTimeout(st, 200*time.Millisecond).DisksOp()(context.Background(), nil)
	var oe *OpError
	if !errors.As(err, &oe) || oe.Code != proto.ErrBusy {
		t.Fatalf("harus busy, dapat %v", err)
	}
}

// storageWithTimeout membuat salinan st dengan batas waktu lain tetapi semaphore
// yang sama.
func storageWithTimeout(st *Storage, timeout time.Duration) *Storage {
	t := st.t
	t.Timeout = timeout
	return &Storage{t: t, sem: st.sem, smart: map[string]*smartEntry{}}
}
