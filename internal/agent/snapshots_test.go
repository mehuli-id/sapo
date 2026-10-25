package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestSnapPolicyEncoding(t *testing.T) {
	p := &proto.SnapshotPolicy{Hourly: 24, Daily: 14, Weekly: 8, Hour: 3}
	if v := encodeSnapPolicy(p); v != "h24.d14.w8.t3" {
		t.Fatalf("encode = %q", v)
	}
	if got, ok := decodeSnapPolicy("h24.d14.w8.t3"); !ok || got != *p {
		t.Fatalf("decode = %+v %v", got, ok)
	}
	for _, bad := range []string{"-", "off", "h999.d1.w1.t1", "h1.d1.w1.t24", "h1.d1.w1.t1;rm", "h01.d1.w1.t1"} {
		if _, ok := decodeSnapPolicy(bad); ok {
			t.Errorf("%q harus ditolak", bad)
		}
	}
	if encodeSnapPolicy(nil) != "off" || encodeSnapPolicy(&proto.SnapshotPolicy{Hour: 5}) != "off" {
		t.Fatal("jadwal kosong = off")
	}
}

func TestSnapPeriodStart(t *testing.T) {
	loc := time.FixedZone("WIB", 7*3600)
	p := proto.SnapshotPolicy{Hour: 3}
	// Rabu 28 Okt 2026 02:30 WIB.
	now := time.Date(2026, 10, 28, 2, 30, 0, 0, loc)
	for kind, want := range map[string]time.Time{
		"hourly": time.Date(2026, 10, 28, 2, 0, 0, 0, loc),
		"daily":  time.Date(2026, 10, 27, 3, 0, 0, 0, loc), // belum jam 3: kemarin
		"weekly": time.Date(2026, 10, 26, 3, 0, 0, 0, loc), // Senin
	} {
		if got := periodStart(kind, p, now, loc); !got.Equal(want) {
			t.Errorf("%s = %v, mau %v", kind, got, want)
		}
	}
	// Senin 26 Okt 01:00: Senin jam 3 belum lewat, jadi Senin sebelumnya.
	mon := time.Date(2026, 10, 26, 1, 0, 0, 0, loc)
	if got := periodStart("weekly", p, mon, loc); !got.Equal(time.Date(2026, 10, 19, 3, 0, 0, 0, loc)) {
		t.Errorf("weekly Senin pagi = %v", got)
	}
}

// testSnapshots: zfs tiruan yang mencatat perintah dan mengembalikan daftar
// dataset dan snapshot dari file.
func testSnapshots(t *testing.T, datasets, snapshots string, pools ...string) (*Snapshots, string) {
	t.Helper()
	s, _, _ := testShares(t, "0")
	dir := t.TempDir()
	log := filepath.Join(dir, "zfs.log")
	_ = os.WriteFile(filepath.Join(dir, "ds"), []byte(datasets), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "snaps"), []byte(snapshots), 0o644)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\n" +
		"case \"$*\" in\n" +
		"'list -H -p -t filesystem'*) cat '" + filepath.Join(dir, "ds") + "' ;;\n" +
		"'list -H -p -t snapshot'*) cat '" + filepath.Join(dir, "snaps") + "' ;;\n" +
		"esac\n"
	zfs := filepath.Join(dir, "zfs")
	_ = os.WriteFile(zfs, []byte(script), 0o755)
	s.t.Zfs = zfs
	pl := "tank\tONLINE\t10\n"
	if len(pools) > 0 {
		pl = pools[0]
	}
	zpool := filepath.Join(dir, "zpool")
	_ = os.WriteFile(zpool, []byte("#!/bin/sh\nprintf '"+strings.ReplaceAll(pl, "\t", "\\t")+"'\n"), 0o755)
	sn := NewSnapshots(s, SnapshotTools{Zpool: zpool, MaxPoolCapacity: 90, CallTimeout: 5 * time.Second})
	sn.location = func() *time.Location { return time.UTC }
	return sn, log
}

func TestSnapshotsTick(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 5, 0, 0, time.UTC)
	old := func(h int) string { return now.Add(-time.Duration(h) * time.Hour).Format(snapTimeFormat) }
	sec := func(h int) string { return strconv.FormatInt(now.Add(-time.Duration(h)*time.Hour).Unix(), 10) }
	datasets := strings.Join([]string{
		"tank/keluarga\th2.d0.w0.t3\tkeluarga\t-",
		"tank/sapo-home/budi\th0.d1.w0.t3\t-\tbudi",
		// Tanpa penanda Sapo / jadwal mati / properti aneh: tidak disentuh.
		"tank/lain\th2.d0.w0.t3\t-\t-",
		"tank/mati\toff\tmati\t-",
		"tank/aneh\th2;x\taneh\t-",
	}, "\n") + "\n"
	snaps := strings.Join([]string{
		"tank/keluarga@sapo-hourly_GMT-" + old(1) + "\t" + sec(1) + "\t0\thourly",
		"tank/keluarga@sapo-hourly_GMT-" + old(2) + "\t" + sec(2) + "\t0\thourly",
		"tank/keluarga@sapo-hourly_GMT-" + old(3) + "\t" + sec(3) + "\t0\thourly",
		// Bernama pola Sapo tetapi dibuat dari terminal (tanpa penanda).
		"tank/keluarga@sapo-hourly_GMT-" + old(4) + "\t" + sec(4) + "\t0\t-",
		"tank/keluarga@sapo-manual_GMT-" + old(30) + "\t" + sec(30) + "\t0\tmanual",
		"tank/keluarga@buatan-admin\t" + sec(40) + "\t0\t-",
		// Harian hari ini sudah ada (setelah jam 3).
		"tank/sapo-home/budi@sapo-daily_GMT-" + old(6) + "\t" + sec(6) + "\t0\tdaily",
	}, "\n") + "\n"
	sn, log := testSnapshots(t, datasets, snaps)
	sn.now = func() time.Time { return now }
	sn.Tick(context.Background())
	calls, _ := os.ReadFile(log)
	c := string(calls)
	// keluarga: jam ini belum ada → diambil; simpan 2 jam-an → 2 terlama
	// dibuang; manual dan snapshot buatan admin tidak disentuh.
	want := "snapshot -o sapo:auto=hourly -- tank/keluarga@sapo-hourly_GMT-" + now.Format(snapTimeFormat)
	if !strings.Contains(c, want) {
		t.Fatalf("snapshot jam ini tidak diambil:\n%s", c)
	}
	if !strings.Contains(c, "destroy -- tank/keluarga@sapo-hourly_GMT-"+old(2)+",sapo-hourly_GMT-"+old(3)+"\n") {
		t.Errorf("snapshot 2 dan 3 jam lalu harus dibuang (satu perintah):\n%s", c)
	}
	for _, line := range strings.Split(c, "\n") {
		if strings.HasPrefix(line, "snapshot") || strings.HasPrefix(line, "destroy") {
			for _, bad := range []string{"tank/lain", "tank/mati", "tank/aneh", "sapo-home/budi", "manual", "buatan-admin", "GMT-" + old(1) + ",", "GMT-" + old(1) + "\n", old(4)} {
				if strings.Contains(line, bad) {
					t.Errorf("tidak boleh: %s", line)
				}
			}
		}
	}
}

func TestSnapshotsOps(t *testing.T) {
	s, _, _ := testShares(t, "0")
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "keluarga", Pool: "tank", Read: []string{}, Write: []string{"user:budi"},
		Snapshots: &proto.SnapshotPolicy{Hourly: 24, Daily: 7, Hour: 2}}); err != nil {
		t.Fatal(err)
	}
	props, _ := s.zfsProps(context.Background(), "tank/keluarga", snapPropName)
	if props[snapPropName] != "h24.d7.w0.t2" {
		t.Fatalf("sapo:snap = %q", props[snapPropName])
	}
	// Jadwal tidak valid: ditolak.
	var oe *OpError
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "keluarga", Pool: "tank", Snapshots: &proto.SnapshotPolicy{Hourly: 9999}}); !errors.As(err, &oe) {
		t.Fatalf("jadwal tidak valid: %v", err)
	}
	// Dimatikan: off (snapshot lama tidak dihapus).
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "keluarga", Pool: "tank", Read: []string{}, Write: []string{"user:budi"}}); err != nil {
		t.Fatal(err)
	}
	props, _ = s.zfsProps(context.Background(), "tank/keluarga", snapPropName)
	if props[snapPropName] != "off" {
		t.Fatalf("sapo:snap = %q", props[snapPropName])
	}
	sn := NewSnapshots(s, SnapshotTools{})
	call := func(h Handler, p any) error {
		raw, _ := json.Marshal(p)
		_, err := h(context.Background(), raw)
		return err
	}
	for name, p := range map[string]proto.SnapshotParams{
		"nama bukan buatan Sapo": {SnapshotTarget: proto.SnapshotTarget{Pool: "tank", Share: "keluarga"}, Name: "buatan-admin"},
		"nama dengan @":          {SnapshotTarget: proto.SnapshotTarget{Pool: "tank", Share: "keluarga"}, Name: "x@y"},
		"folder bukan Sapo":      {SnapshotTarget: proto.SnapshotTarget{Pool: "tank", Share: "lain"}, Name: "sapo-manual_GMT-2026.10.28-10.00.00"},
		"share dan user":         {SnapshotTarget: proto.SnapshotTarget{Pool: "tank", Share: "keluarga", User: "budi"}, Name: "sapo-manual_GMT-2026.10.28-10.00.00"},
	} {
		if err := call(sn.DeleteOp(), p); !errors.As(err, &oe) {
			t.Errorf("%s harus ditolak: %v", name, err)
		}
	}
	if err := call(sn.CreateOp(), proto.SnapshotTarget{Pool: "tank", Share: "lain"}); !errors.As(err, &oe) || oe.Code != proto.ErrNotFound {
		t.Fatalf("snapshot folder bukan Sapo: %v", err)
	}
}

func TestSMBConfShadowCopy(t *testing.T) {
	s, _, _ := testShares(t, "0")
	conf := string(s.renderSMBConf(nil, ""))
	for _, w := range []string{"vfs objects = shadow_copy2", "shadow:snapdir = .zfs/snapshot", "shadow:format = _GMT-%Y.%m.%d-%H.%M.%S"} {
		if !strings.Contains(conf, w) {
			t.Errorf("smb.conf tanpa %q", w)
		}
	}
}

// Jam sistem pernah maju (snapshot bertanggal masa depan), pool hampir
// penuh, atau pool tidak sehat: tidak ada snapshot salah ambil dan tidak ada
// yang terbuang.
func TestSnapshotsGuards(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 5, 0, 0, time.UTC)
	future := now.AddDate(5, 0, 0)
	ds := "tank/keluarga\th2.d0.w0.t3\tkeluarga\t-\n"
	snaps := "tank/keluarga@sapo-hourly_GMT-" + future.Format(snapTimeFormat) + "\t" + strconv.FormatInt(future.Unix(), 10) + "\t0\thourly\n"
	sn, log := testSnapshots(t, ds, snaps)
	sn.now = func() time.Time { return now }
	sn.Tick(context.Background())
	c, _ := os.ReadFile(log)
	if !strings.Contains(string(c), "snapshot -o sapo:auto=hourly") || strings.Contains(string(c), "destroy") {
		t.Fatalf("snapshot masa depan harus diabaikan (bukan menghentikan jadwal atau dibuang):\n%s", c)
	}
	for name, pools := range map[string]string{"penuh": "tank\tONLINE\t95\n", "rusak": "tank\tSUSPENDED\t10\n"} {
		sn, log := testSnapshots(t, ds, "", pools)
		sn.now = func() time.Time { return now }
		sn.Tick(context.Background())
		if c, _ := os.ReadFile(log); strings.Contains("\n"+string(c), "\nsnapshot -o") {
			t.Errorf("pool %s: snapshot tidak boleh diambil:\n%s", name, c)
		}
	}
}
