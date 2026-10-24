package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// zpoolFake mengatur jawaban zpool pengganti per subperintah. Setiap
// pemanggilan dicatat di <prog>.calls (argumen per baris, dipisah "---").
type zpoolFake struct {
	list        string // fixture zpool list -H -p (kosong = tidak ada pool)
	listExit    int
	listStderr  string
	names       string // isi zpool list -H -o name
	status      string // fixture zpool status -j
	createExit  int
	scrubExit   int
	scrubStderr string
}

func writeZpool(t *testing.T, dir string, z zpoolFake) string {
	t.Helper()
	prog := filepath.Join(dir, "zpool")
	fixture := func(name string) string {
		if name == "" {
			return "/dev/null"
		}
		abs, err := filepath.Abs(filepath.Join("testdata", "storage", name))
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}
	namesFile := filepath.Join(dir, "names.txt")
	if err := os.WriteFile(namesFile, []byte(z.names), 0o644); err != nil {
		t.Fatal(err)
	}
	q := strconv.Quote
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> '" + prog + ".calls'\necho --- >> '" + prog + ".calls'\n" +
		"case \"$1\" in\n" +
		"list)\n  if [ \"$3\" = -o ] && [ \"$4\" = name ]; then cat '" + namesFile + "'; exit 0; fi\n" +
		"  printf '%s' " + q(z.listStderr) + " >&2\n  cat '" + fixture(z.list) + "'\n  exit " + strconv.Itoa(z.listExit) + " ;;\n" +
		"status) cat '" + fixture(z.status) + "' ;;\n" +
		"create) exit " + strconv.Itoa(z.createExit) + " ;;\n" +
		"scrub) printf '%s' " + q(z.scrubStderr) + " >&2; exit " + strconv.Itoa(z.scrubExit) + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return prog
}

// zpoolCalls mengembalikan argumen setiap pemanggilan zpool.
func zpoolCalls(t *testing.T, prog string) [][]string {
	t.Helper()
	b, err := os.ReadFile(prog + ".calls")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, call := range strings.Split(strings.TrimSuffix(string(b), "---\n"), "---\n") {
		out = append(out, strings.Split(strings.TrimRight(call, "\n"), "\n"))
	}
	return out
}

var testLayouts = []proto.PoolLayout{
	{ID: proto.LayoutSingle, MinDisks: 1, MaxDisks: 1},
	{ID: proto.LayoutMirror, MinDisks: 2, MaxDisks: 4},
	{ID: proto.LayoutRAIDZ1, MinDisks: 3, MaxDisks: 8},
}

func newTestPools(t *testing.T, z zpoolFake) (*Pools, string) {
	t.Helper()
	dir := t.TempDir()
	st := NewStorage(StorageTools{
		Lsblk: fakeTool(t, dir, "lsblk", "lsblk-pool.json", 0), Smartctl: "/nonexistent/smartctl",
		Timeout: 5 * time.Second, MaxConcurrent: 2,
	})
	zpool := writeZpool(t, dir, z)
	return NewPools(st, PoolTools{
		Zpool: zpool, MountBase: "/srv/sapo", Ashift: 12,
		RootProperties: map[string]string{"compression": "lz4", "atime": "off"},
		Layouts:        testLayouts, CreateTimeout: 5 * time.Second, ByIDDir: filepath.Join(dir, "by-id"),
	}), zpool
}

func TestDiskUsage(t *testing.T) {
	data, err := os.ReadFile("testdata/storage/lsblk-pool.json")
	if err != nil {
		t.Fatal(err)
	}
	disks, err := parseLsblk(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range disks {
		got[d.Name] = d.Usage + "/" + d.UsageReason
	}
	want := map[string]string{
		"vda": "in_use/mounted", // disk sistem
		"vdb": "in_use/pool", "vdc": "in_use/pool",
		"vdd": "free/", "vdf": "free/",
		"vde": "data/", // ext4 yang tidak terpasang: boleh dipakai bila penghapusan dikonfirmasi
		"vdg": "in_use/swap", "vdh": "in_use/lvm",
		"sdi": "free/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage = %v\nharus   %v", got, want)
	}

	// Fixture lama: md0 terpasang di atas sdb1 (RAID) = sedang dipakai.
	data, _ = os.ReadFile("testdata/storage/lsblk.json")
	disks, _ = parseLsblk(data, []string{"zram*"})
	for _, d := range disks {
		if d.Name == "sdb" && (d.Usage != proto.DiskInUse || d.UsageReason != proto.UsageMounted) {
			t.Fatalf("sdb = %s/%s, harus in_use/mounted", d.Usage, d.UsageReason)
		}
	}
}

func TestPoolsListParsesStatus(t *testing.T) {
	p, _ := newTestPools(t, zpoolFake{list: "zpool-list.txt", status: "zpool-status.json"})
	res, err := p.PoolsOp()(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.(proto.PoolsResult)
	if !out.Available || len(out.Pools) != 2 || !reflect.DeepEqual(out.Layouts, testLayouts) {
		t.Fatalf("hasil = %+v", out)
	}
	// Urutan nama: backup, tank.
	backup, tank := out.Pools[0], out.Pools[1]
	if tank.Name != "tank" || tank.Health != "ONLINE" || tank.SizeBytes != 4294967296 || tank.Layout != proto.LayoutMirror {
		t.Fatalf("tank = %+v", tank)
	}
	if len(tank.Vdevs) != 1 || len(tank.Vdevs[0].Children) != 2 {
		t.Fatalf("vdev tank = %+v", tank.Vdevs)
	}
	vdb, vdc := tank.Vdevs[0].Children[0], tank.Vdevs[0].Children[1]
	if vdb.Name != "vdb" || vdb.Disk != "vdb" || vdc.Disk != "vdc" || vdc.ChecksumErrors != 2 {
		t.Fatalf("disk tank = %+v %+v", vdb, vdc)
	}
	if s := tank.Scan; s == nil || s.Function != "scrub" || s.State != "finished" || s.EndTime != 1792800060 || s.Paused {
		t.Fatalf("scrub tank = %+v", tank.Scan)
	}

	if backup.Health != "DEGRADED" || backup.Layout != proto.LayoutRAIDZ2 || backup.CapacityPct != 50 || backup.ErrorCount != 4 || backup.Status == "" || backup.Action == "" {
		t.Fatalf("backup = %+v", backup)
	}
	missing := backup.Vdevs[0].Children[0]
	if missing.State != "UNAVAIL" || missing.Disk != "" {
		t.Fatalf("disk hilang = %+v", missing)
	}
	if s := backup.Scan; s == nil || s.Function != "resilver" || s.State != "scanning" || s.Examined != 3221225472 {
		t.Fatalf("resilver = %+v", backup.Scan)
	}
}

func TestPoolsUnavailable(t *testing.T) {
	// Modul kernel tidak termuat: zpool keluar dengan error.
	p, _ := newTestPools(t, zpoolFake{listExit: 1, listStderr: "The ZFS modules cannot be auto-loaded.\nTry running 'modprobe zfs' as root.\n"})
	res, err := p.PoolsOp()(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := res.(proto.PoolsResult); out.Available || !strings.Contains(out.Message, "ZFS modules") || out.Pools == nil {
		t.Fatalf("hasil = %+v", out)
	}
	// zfsutils belum terpasang.
	p.t.Zpool = "/nonexistent/zpool"
	res, _ = p.PoolsOp()(context.Background(), nil)
	if out := res.(proto.PoolsResult); out.Available || !strings.Contains(out.Message, "zfsutils") {
		t.Fatalf("hasil = %+v", out)
	}
}

func ref(name, serial string, size uint64, erase bool) proto.DiskRef {
	return proto.DiskRef{Name: name, Serial: serial, SizeBytes: size, Erase: erase}
}

const gib4 = 4294967296

func createParams(t *testing.T, v proto.PoolCreateParams) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPoolCreateMirror(t *testing.T) {
	p, zpool := newTestPools(t, zpoolFake{names: "tank\n"})
	// Nama by-id yang stabil dipakai bila ada (WWN didahulukan).
	byID := p.t.ByIDDir
	devDir := t.TempDir()
	for _, n := range []string{"vdd", "vde"} {
		if err := os.WriteFile(filepath.Join(devDir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(byID, 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"virtio-NEW1": "vdd", "wwn-0x5000c500a1b2c3d4": "vdd", "virtio-NEW1-part1": "vdd",
		"virtio-OLD1": "vde",
	} {
		if err := os.Symlink(filepath.Join(devDir, target), filepath.Join(byID, link)); err != nil {
			t.Fatal(err)
		}
	}

	_, err := p.PoolCreateOp()(context.Background(), createParams(t, proto.PoolCreateParams{
		Name: "data", Layout: proto.LayoutMirror,
		Disks: []proto.DiskRef{ref("vdd", "NEW1", gib4, false), ref("vde", "OLD1", gib4, true)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var create []string
	for _, c := range zpoolCalls(t, zpool) {
		if c[0] == "create" {
			create = c
		}
	}
	want := []string{
		"create", "-f", "-o", "ashift=12", "-O", "atime=off", "-O", "compression=lz4", "-m", "/srv/sapo/data", "--",
		"data", "mirror", filepath.Join(byID, "wwn-0x5000c500a1b2c3d4"), filepath.Join(byID, "virtio-OLD1"),
	}
	// -f karena vde berisi data yang penghapusannya dikonfirmasi.
	if !reflect.DeepEqual(create, want) {
		t.Fatalf("zpool create = %q\nharus          %q", create, want)
	}
}

func TestPoolCreateWithoutForceOnFreeDisks(t *testing.T) {
	p, zpool := newTestPools(t, zpoolFake{})
	if _, err := p.PoolCreateOp()(context.Background(), createParams(t, proto.PoolCreateParams{
		Name: "solo", Layout: proto.LayoutSingle, Disks: []proto.DiskRef{ref("vdd", "NEW1", gib4, false)},
	})); err != nil {
		t.Fatal(err)
	}
	for _, c := range zpoolCalls(t, zpool) {
		if c[0] == "create" {
			if strings.Join(c, " ") != "create -o ashift=12 -O atime=off -O compression=lz4 -m /srv/sapo/solo -- solo /dev/vdd" {
				t.Fatalf("zpool create = %q", c)
			}
			return
		}
	}
	t.Fatal("zpool create tidak dipanggil")
}

func TestPoolCreateRejects(t *testing.T) {
	mirror := func(a, b proto.DiskRef) proto.PoolCreateParams {
		return proto.PoolCreateParams{Name: "data", Layout: proto.LayoutMirror, Disks: []proto.DiskRef{a, b}}
	}
	vdd := ref("vdd", "NEW1", gib4, false)
	cases := []struct {
		name   string
		params proto.PoolCreateParams
		code   string
	}{
		{"data tanpa konfirmasi", mirror(vdd, ref("vde", "OLD1", gib4, false)), proto.ErrDiskHasData},
		{"disk pool lain", mirror(vdd, ref("vdb", "POOL1", gib4, true)), proto.ErrDiskInUse},
		{"disk sistem", mirror(vdd, ref("vda", "SYS0", 21474836480, true)), proto.ErrDiskInUse},
		{"swap", mirror(vdd, ref("vdg", "SWAP1", gib4, true)), proto.ErrDiskInUse},
		{"LVM", mirror(vdd, ref("vdh", "LVM1", gib4, true)), proto.ErrDiskInUse},
		{"flashdisk", mirror(vdd, ref("sdi", "USB1", 15728640000, false)), proto.ErrDiskRemovable},
		{"nomor seri lain", mirror(vdd, ref("vdf", "LAIN", 8589934592, false)), proto.ErrDiskChanged},
		{"ukuran lain", mirror(vdd, ref("vdf", "BIG1", gib4, false)), proto.ErrDiskChanged},
		{"disk hilang", mirror(vdd, ref("vdz", "X", gib4, false)), proto.ErrDiskChanged},
		{"nama sudah ada", proto.PoolCreateParams{Name: "tank", Layout: proto.LayoutSingle, Disks: []proto.DiskRef{vdd}}, proto.ErrPoolExists},
		{"dipilih dua kali", mirror(vdd, vdd), proto.ErrInvalidParams},
		{"kurang disk", proto.PoolCreateParams{Name: "data", Layout: proto.LayoutRAIDZ1, Disks: []proto.DiskRef{vdd}}, proto.ErrInvalidParams},
		{"susunan tidak tersedia", proto.PoolCreateParams{Name: "data", Layout: proto.LayoutRAIDZ3, Disks: []proto.DiskRef{vdd}}, proto.ErrInvalidParams},
		{"nama disk aneh", mirror(vdd, ref("../sda", "", 0, false)), proto.ErrInvalidParams},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, zpool := newTestPools(t, zpoolFake{names: "tank\n"})
			_, err := p.PoolCreateOp()(context.Background(), createParams(t, tc.params))
			var oe *OpError
			if !errors.As(err, &oe) || oe.Code != tc.code {
				t.Fatalf("err = %v, harus kode %s", err, tc.code)
			}
			for _, c := range zpoolCalls(t, zpool) {
				if c[0] == "create" {
					t.Fatal("zpool create tidak boleh dipanggil")
				}
			}
		})
	}
}

func TestPoolNames(t *testing.T) {
	for name, ok := range map[string]bool{
		"data": true, "tank_2": true, "a": true, "c": true, "cadangan": true,
		"": false, "Data": false, "2data": false, "-data": false, "da ta": false, "data/x": false,
		"mirror": false, "mirror2": false, "raidz": false, "draidx": false, "spare1": false, "log": false, "c0": false, "c1d0": false,
		strings.Repeat("a", 33): false,
	} {
		if validPoolName(name) != ok {
			t.Errorf("validPoolName(%q) = %v", name, !ok)
		}
	}
}

func TestPoolCreateMixedSizesForces(t *testing.T) {
	p, zpool := newTestPools(t, zpoolFake{})
	if _, err := p.PoolCreateOp()(context.Background(), createParams(t, proto.PoolCreateParams{
		Name: "data", Layout: proto.LayoutMirror,
		Disks: []proto.DiskRef{ref("vdd", "NEW1", gib4, false), ref("vdf", "BIG1", 8589934592, false)},
	})); err != nil {
		t.Fatal(err)
	}
	for _, c := range zpoolCalls(t, zpool) {
		if c[0] == "create" && c[1] != "-f" {
			t.Fatalf("ukuran berbeda butuh -f: %q", c)
		}
	}
}

func TestPoolCreateFailureAndBusy(t *testing.T) {
	p, _ := newTestPools(t, zpoolFake{createExit: 1})
	params := createParams(t, proto.PoolCreateParams{Name: "solo", Layout: proto.LayoutSingle, Disks: []proto.DiskRef{ref("vdd", "NEW1", gib4, false)}})
	_, err := p.PoolCreateOp()(context.Background(), params)
	var oe *OpError
	if !errors.As(err, &oe) || oe.Code != "pool_create_failed" {
		t.Fatalf("err = %v", err)
	}
	// Hanya satu operasi pool pada satu waktu.
	p.write.Lock()
	defer p.write.Unlock()
	if _, err := p.PoolCreateOp()(context.Background(), params); !errors.As(err, &oe) || oe.Code != proto.ErrBusy {
		t.Fatalf("err = %v, harus busy", err)
	}
}

func TestPoolScrub(t *testing.T) {
	scrub := func(z zpoolFake, name, action string) (*Pools, string, error) {
		p, zpool := newTestPools(t, z)
		raw, _ := json.Marshal(proto.PoolScrubParams{Name: name, Action: action})
		_, err := p.PoolScrubOp()(context.Background(), raw)
		return p, zpool, err
	}
	_, zpool, err := scrub(zpoolFake{names: "tank\n"}, "tank", "start")
	if err != nil {
		t.Fatal(err)
	}
	calls := zpoolCalls(t, zpool)
	if last := calls[len(calls)-1]; strings.Join(last, " ") != "scrub -- tank" {
		t.Fatalf("zpool = %q", last)
	}
	_, zpool, err = scrub(zpoolFake{names: "tank\n"}, "tank", "stop")
	if calls = zpoolCalls(t, zpool); err != nil || strings.Join(calls[len(calls)-1], " ") != "scrub -s -- tank" {
		t.Fatalf("stop: %v %q", err, calls)
	}
	// Sudah berjalan / tidak ada scrub: dianggap berhasil.
	if _, _, err := scrub(zpoolFake{names: "tank\n", scrubExit: 1, scrubStderr: "cannot scrub tank: currently scrubbing; use 'zpool scrub -s' to cancel"}, "tank", "start"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scrub(zpoolFake{names: "tank\n", scrubExit: 1, scrubStderr: "cannot cancel scrubbing tank: there is no active scrub"}, "tank", "stop"); err != nil {
		t.Fatal(err)
	}
	var oe *OpError
	if _, _, err := scrub(zpoolFake{names: "tank\n", scrubExit: 1, scrubStderr: "cannot scrub tank: currently resilvering"}, "tank", "start"); !errors.As(err, &oe) || oe.Code != proto.ErrPoolBusy {
		t.Fatalf("resilver: %v", err)
	}
	if _, _, err := scrub(zpoolFake{names: "tank\n"}, "lain", "start"); !errors.As(err, &oe) || oe.Code != proto.ErrNotFound {
		t.Fatalf("pool tidak ada: %v", err)
	}
	for _, bad := range [][2]string{{"tank", "pause"}, {"-s", "start"}, {"Tank", "start"}} {
		if _, _, err := scrub(zpoolFake{names: "tank\n"}, bad[0], bad[1]); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}
