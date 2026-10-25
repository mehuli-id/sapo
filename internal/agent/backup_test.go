package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// fakeZFS: dataset (dengan properti) dan snapshot dalam memori.
type fakeZFS struct {
	mu    sync.Mutex
	ds    map[string]map[string]string
	snaps map[string]time.Time // ds@nama → waktu
	calls []string
	now   time.Time
}

func (f *fakeZFS) run(_ context.Context, flags []string, ops ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(append(append([]string{}, flags...), ops...), " "))
	switch flags[0] {
	case "snapshot":
		f.now = f.now.Add(time.Minute)
		f.snaps[ops[0]] = f.now
	case "destroy":
		if len(flags) > 1 && flags[1] == "-r" {
			for k := range f.snaps {
				if strings.HasPrefix(k, ops[0]+"@") {
					delete(f.snaps, k)
				}
			}
			delete(f.ds, ops[0])
			return "", nil
		}
		ds, names, _ := strings.Cut(ops[0], "@")
		for _, n := range strings.Split(names, ",") {
			delete(f.snaps, ds+"@"+n)
		}
	case "create":
		props := map[string]string{"type": "filesystem"}
		for i := 1; i+1 < len(flags); i += 2 {
			k, v, _ := strings.Cut(flags[i+1], "=")
			props[k] = v
		}
		f.ds[ops[0]] = props
	case "list":
		ds := ops[0]
		var keys []string
		for k := range f.snaps {
			if strings.HasPrefix(k, ds+"@") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			b.WriteString(k + "\t" + itoa64(f.snaps[k].Unix()) + "\t" + itoa64(f.snaps[k].UnixNano()) + "\n")
		}
		return b.String(), nil
	}
	return "", nil
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }

func (f *fakeZFS) props(_ context.Context, ds string, names ...string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.ds[ds]
	if !ok {
		return nil, errNoDataset
	}
	out := map[string]string{}
	for _, n := range names {
		if v, ok := p[n]; ok {
			out[n] = v
		} else {
			out[n] = "-"
		}
	}
	return out, nil
}

// pipe: receive membuat dataset tujuan (dengan -o) dan menyalin snapshot.
func (f *fakeZFS) pipe(_ context.Context, send, recv []string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "send "+strings.Join(send, " ")+" | "+strings.Join(recv, " "))
	src := send[len(send)-1]
	dst := recv[len(recv)-1]
	_, snap, _ := strings.Cut(src, "@")
	if _, ok := f.ds[dst]; !ok {
		props := map[string]string{"type": "filesystem"}
		for i := 0; i+1 < len(recv); i++ {
			if recv[i] == "-o" {
				k, v, _ := strings.Cut(recv[i+1], "=")
				props[k] = v
			}
		}
		f.ds[dst] = props
	}
	f.snaps[dst+"@"+snap] = f.snaps[src]
	return 1000, nil
}

func testBackups(t *testing.T) (*Backups, *fakeZFS) {
	t.Helper()
	s, _, _ := testShares(t, "0")
	f := &fakeZFS{ds: map[string]map[string]string{
		"tank":          {"type": "filesystem"},
		"tank/keluarga": {"type": "filesystem", sharePropName: "keluarga"},
		"usb":           {"type": "filesystem"},
	}, snaps: map[string]time.Time{}, now: time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)}
	b := NewBackups(BackupTools{StateFile: filepath.Join(t.TempDir(), "backup.json")}, s)
	b.zfsFn, b.propsFn, b.pipe = f.run, f.props, f.pipe
	b.poolsFn = func(context.Context) (map[string]bool, error) { return map[string]bool{"tank": true, "usb": true}, nil }
	b.loc = func() *time.Location { return time.UTC }
	clock := time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)
	b.now = func() time.Time { clock = clock.Add(time.Hour); return clock }
	return b, f
}

func bkCall(t *testing.T, h Handler, p any) error {
	t.Helper()
	raw, _ := json.Marshal(p)
	_, err := h(context.Background(), raw)
	return err
}

func TestBackupsFullThenIncremental(t *testing.T) {
	b, f := testBackups(t)
	job := proto.BackupJob{ID: "keluarga-usb", SrcPool: "tank", Share: "keluarga", DestPool: "usb", Hour: 2, Keep: 2, Enabled: true}
	var oe *OpError
	for name, j := range map[string]proto.BackupJob{
		"pool sama":     {ID: "x1", SrcPool: "tank", Share: "keluarga", DestPool: "tank", Hour: 2, Keep: 2},
		"bukan folder":  {ID: "x2", SrcPool: "tank", Share: "lain", DestPool: "usb", Hour: 2, Keep: 2},
		"tujuan hilang": {ID: "x3", SrcPool: "tank", Share: "keluarga", DestPool: "hantu", Hour: 2, Keep: 2},
		"jam aneh":      {ID: "x4", SrcPool: "tank", Share: "keluarga", DestPool: "usb", Hour: 30, Keep: 2},
	} {
		if err := bkCall(t, b.SetOp(), j); !errors.As(err, &oe) {
			t.Errorf("%s harus ditolak: %v", name, err)
		}
	}
	if err := bkCall(t, b.SetOp(), job); err != nil {
		t.Fatal(err)
	}
	// Tiga kali backup: penuh, lalu dua inkremental; tujuan menyimpan 2.
	for i := 0; i < 3; i++ {
		if err := b.execute(context.Background(), job.ID); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}
	all := strings.Join(f.calls, "\n")
	if !strings.Contains(all, "receive -u -o canmount=noauto -o readonly=on -o sapo:backup=keluarga-usb -- usb/sapo-backup/keluarga-usb") {
		t.Fatalf("backup penuh:\n%s", all)
	}
	if strings.Count(all, "send -L -c -i @sapo-backup-keluarga-usb_GMT-") != 2 {
		t.Fatalf("dua backup inkremental:\n%s", all)
	}
	if strings.Contains(all, "receive -u -F") || strings.Contains(all, "destroy -r -- tank") {
		t.Fatalf("tidak boleh menimpa tujuan atau menghapus sumber:\n%s", all)
	}
	src, dst := 0, 0
	for k := range f.snaps {
		switch {
		case strings.HasPrefix(k, "tank/keluarga@"):
			src++
		case strings.HasPrefix(k, "usb/sapo-backup/keluarga-usb@"):
			dst++
		}
	}
	if src != 1 || dst != 2 {
		t.Fatalf("snapshot sumber %d (mau 1), tujuan %d (mau 2): %v", src, dst, f.snaps)
	}
	// Folder yang di-backup dan pool-nya tidak bisa dihapus.
	if id := b.usingShare("tank", "keluarga"); id != job.ID {
		t.Fatalf("usingShare = %q", id)
	}
	if err := b.refusePool(context.Background(), "usb"); !errors.As(err, &oe) {
		t.Fatalf("pool tujuan: %v", err)
	}
	// Daftar.
	res, err := b.ListOp()(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	l := res.(proto.BackupsListResult)
	if len(l.Jobs) != 1 || !l.Jobs[0].LastOK || len(l.Jobs[0].Copies) != 2 || !l.Jobs[0].DestExists {
		t.Fatalf("daftar = %+v", l)
	}
	// Pulihkan menjadi folder baru (folder yang ada tidak ditimpa).
	if err := bkCall(t, b.RestoreOp(), proto.BackupRestoreParams{ID: job.ID, Copy: l.Jobs[0].Copies[0].Name, Name: "keluarga"}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("pulihkan ke folder yang ada: %v", err)
	}
	if err := bkCall(t, b.RestoreOp(), proto.BackupRestoreParams{ID: job.ID, Copy: "buatan-admin", Name: "pulih"}); !errors.As(err, &oe) {
		t.Fatalf("salinan aneh: %v", err)
	}
	if err := bkCall(t, b.RestoreOp(), proto.BackupRestoreParams{ID: job.ID, Copy: l.Jobs[0].Copies[0].Name, Name: "pulih"}); err != nil {
		t.Fatal(err)
	}
	b.runMu.Lock() // menunggu pemulihan di latar belakang selesai
	b.runMu.Unlock()
	if p := f.ds["tank/pulih"]; p == nil || p[sharePropName] != "pulih" {
		t.Fatalf("folder pulihan = %+v\n%s", p, strings.Join(f.calls, "\n"))
	}
	// Klaim pendaftaran: sekali saja.
	claim := func() bool {
		raw, _ := json.Marshal(proto.BackupClaimParams{Pool: "tank", Name: "pulih"})
		out, err := b.ClaimOp()(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		return out.(map[string]bool)["claimed"]
	}
	if !claim() || claim() {
		t.Fatal("klaim folder pulihan harus berhasil tepat sekali")
	}
	// Hapus tugas beserta salinan.
	if err := bkCall(t, b.DeleteOp(), proto.BackupDeleteParams{ID: job.ID, DeleteData: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.ds["usb/sapo-backup/keluarga-usb"]; ok {
		t.Fatal("salinan harus terhapus")
	}
	if b.usingShare("tank", "keluarga") != "" {
		t.Fatal("tugas harus terhapus")
	}
}

// Salinan di tujuan tanpa dasar yang sama dengan sumber: tidak pernah
// ditimpa otomatis.
func TestBackupDiverged(t *testing.T) {
	b, f := testBackups(t)
	job := proto.BackupJob{ID: "kel", SrcPool: "tank", Share: "keluarga", DestPool: "usb", Hour: 2, Keep: 3, Enabled: true}
	if err := bkCall(t, b.SetOp(), job); err != nil {
		t.Fatal(err)
	}
	if err := b.execute(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	for k := range f.snaps {
		if strings.HasPrefix(k, "tank/keluarga@") {
			delete(f.snaps, k)
		}
	}
	var oe *OpError
	if err := b.execute(context.Background(), job.ID); !errors.As(err, &oe) || oe.Code != "backup_diverged" {
		t.Fatalf("tanpa dasar bersama: %v", err)
	}
}

func TestBackupDue(t *testing.T) {
	b, _ := testBackups(t)
	b.loc = func() *time.Location { return time.UTC }
	now := time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)
	j := &backupJobState{BackupJob: proto.BackupJob{Hour: 2, Enabled: true}}
	if !b.due(j, now) {
		t.Fatal("belum pernah: harus jalan")
	}
	j.LastRun = now.Add(-30 * time.Minute)
	if b.due(j, now) {
		t.Fatal("sudah berhasil hari ini")
	}
	j.LastRun, j.LastAttempt = time.Time{}, now.Add(-10*time.Minute)
	if b.due(j, now) {
		t.Fatal("gagal 10 menit lalu: tunggu sejam")
	}
	j.Enabled = false
	j.LastAttempt = time.Time{}
	if b.due(j, now) {
		t.Fatal("tugas mati")
	}
}

// Dasar harus snapshot terbaru di tujuan; snapshot yang sudah diterima walau
// proses melapor gagal tidak dibuang dari sumber.
func TestBackupBaseAndFailedReport(t *testing.T) {
	b, f := testBackups(t)
	job := proto.BackupJob{ID: "kel", SrcPool: "tank", Share: "keluarga", DestPool: "usb", Hour: 2, Keep: 5, Enabled: true}
	if err := bkCall(t, b.SetOp(), job); err != nil {
		t.Fatal(err)
	}
	if err := b.execute(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	// Stream diterima, lalu "zfs send" melapor gagal.
	pipe := b.pipe
	b.pipe = func(ctx context.Context, send, recv []string) (uint64, error) {
		n, _ := pipe(ctx, send, recv)
		return n, errors.New("zfs send: gagal di akhir")
	}
	if err := b.execute(context.Background(), job.ID); err == nil {
		t.Fatal("galat harus dilaporkan")
	}
	b.pipe = pipe
	src := 0
	for k := range f.snaps {
		if strings.HasPrefix(k, "tank/keluarga@") {
			src++
		}
	}
	if src != 2 {
		t.Fatalf("snapshot yang sudah diterima harus tetap di sumber: %v", f.snaps)
	}
	if err := b.execute(context.Background(), job.ID); err != nil {
		t.Fatalf("backup berikutnya dari dasar terbaru: %v", err)
	}
	// Snapshot terbaru tujuan hilang dari sumber: berhenti (bukan memakai
	// dasar lebih lama yang akan ditolak receive).
	var newest string
	for k := range f.snaps {
		if strings.HasPrefix(k, "usb/sapo-backup/kel@") && k > newest {
			newest = k
		}
	}
	_, name, _ := strings.Cut(newest, "@")
	delete(f.snaps, "tank/keluarga@"+name)
	var oe *OpError
	if err := b.execute(context.Background(), job.ID); !errors.As(err, &oe) || oe.Code != "backup_diverged" {
		t.Fatalf("dasar terbaru hilang: %v", err)
	}
}
