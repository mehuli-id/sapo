package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

type fakeRestic struct {
	mu    sync.Mutex
	calls [][]string
	// fail: galat untuk subperintah restic tertentu (init, backup, …).
	fail map[string]error
	out  map[string]string
}

func (f *fakeRestic) exec(_ context.Context, unit string, argv []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argv)
	i := 0
	for i < len(argv) && argv[i] != "--" {
		i++
	}
	// Subperintah restic: setelah --repo X --password-file Y [-o …].
	rest := argv[i+1:]
	sub := ""
	for j := 1; j < len(rest); j++ {
		if rest[j] == "--repo" || rest[j] == "--password-file" || rest[j] == "-o" {
			j++
			continue
		}
		sub = rest[j]
		if sub == "cat" {
			sub = "cat"
		}
		break
	}
	if err := f.fail[sub]; err != nil {
		return nil, err
	}
	return []byte(f.out[sub]), nil
}

func (f *fakeRestic) find(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if hasSub(c, sub) {
			return c
		}
	}
	return nil
}

func hasSub(argv []string, sub string) bool {
	for i, a := range argv {
		if a == "--" {
			for _, b := range argv[i+1:] {
				if b == sub {
					return true
				}
			}
		}
	}
	return false
}

func testCloud(t *testing.T) (*Cloud, *fakeRestic, *fakeZFS, *Backups) {
	t.Helper()
	b, f := testBackups(t)
	dir := t.TempDir()
	restic := filepath.Join(dir, "restic")
	_ = os.WriteFile(restic, []byte("#!/bin/sh\n"), 0o755)
	c := NewCloud(CloudTools{Restic: restic, SystemdRun: "/usr/bin/systemd-run", Systemctl: "/usr/bin/systemctl",
		StateFile: filepath.Join(dir, "cloud.json"), SecretsDir: filepath.Join(dir, "rahasia"), CacheDir: filepath.Join(dir, "cache")}, b)
	fr := &fakeRestic{fail: map[string]error{}, out: map[string]string{}}
	c.exec = fr.exec
	c.clockOK = func(context.Context) bool { return true }
	b.resetFn = func(string) error { return nil }
	// Snapshot ZFS sementara harus bisa "dibuka".
	b.shares.t.MountBase = filepath.Join(dir, "srv")
	_ = os.MkdirAll(filepath.Join(dir, "srv/tank/keluarga/.zfs/snapshot", cloudSnapName("keluarga-cabcdef")), 0o755)
	return c, fr, f, b
}

func cloudCall(t *testing.T, h Handler, p any) (any, error) {
	t.Helper()
	raw, _ := json.Marshal(p)
	return h(context.Background(), raw)
}

func TestCloudRepoAndBackup(t *testing.T) {
	c, fr, f, b := testCloud(t)
	var oe *OpError
	repo := proto.CloudRepoParams{Name: "Backblaze", Endpoint: "s3.us-west-002.backblazeb2.com", Bucket: "sapo-rumah", Prefix: "nas1",
		Region: "us-west-002", AccessKey: "KUNCI-AKSES", SecretKey: "rahasia/sekali+"}
	for name, bad := range map[string]proto.CloudRepoParams{
		"bucket":        {Name: "x", Endpoint: "s3.example.com", Bucket: "A_B", AccessKey: "a", SecretKey: "b"},
		"endpoint":      {Name: "x", Endpoint: "s3.example.com/../x", Bucket: "abc", AccessKey: "a", SecretKey: "b"},
		"prefix":        {Name: "x", Endpoint: "s3.example.com", Bucket: "abc", Prefix: "../etc", AccessKey: "a", SecretKey: "b"},
		"kunci":         {Name: "x", Endpoint: "s3.example.com", Bucket: "abc", AccessKey: "a b", SecretKey: "b"},
		"kunci newline": {Name: "x", Endpoint: "s3.example.com", Bucket: "abc", AccessKey: "a", SecretKey: "b\naws_session_token = x"},
	} {
		if _, err := cloudCall(t, c.RepoAddOp(), bad); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
			t.Errorf("%s harus ditolak: %v", name, err)
		}
	}
	res, err := cloudCall(t, c.RepoAddOp(), repo)
	if err != nil {
		t.Fatal(err)
	}
	added := res.(proto.CloudRepoAdded)
	if len(added.Password) < 30 || !strings.Contains(added.Password, "-") {
		t.Fatalf("password = %q", added.Password)
	}
	// Rahasia hanya di file 0600, masuk unit lewat LoadCredential.
	for _, p := range []string{c.pwFile(added.ID), c.s3File(added.ID)} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	init := fr.find("init")
	all := strings.Join(init, " ")
	for _, want := range []string{"--property=ProtectSystem=strict", "--property=NoNewPrivileges=yes", "--property=CapabilityBoundingSet= ",
		"--property=LoadCredential=restic-pw:" + c.pwFile(added.ID), "--repo s3:https://s3.us-west-002.backblazeb2.com/sapo-rumah/nas1",
		"-o s3.region=us-west-002", "--property=RestrictAddressFamilies=AF_INET AF_INET6 ", "--property=DynamicUser=yes", "-/run/dbus", "-/run/sapo"} {
		if !strings.Contains(all+" ", want) {
			t.Errorf("init tidak memuat %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "-/run/systemd") {
		t.Fatal("/run/systemd tidak boleh disembunyikan (dipakai systemd sendiri untuk InaccessiblePaths)")
	}
	if strings.Contains(all, "rahasia/sekali") || strings.Contains(all, added.Password) || strings.Contains(all, "KUNCI-AKSES") {
		t.Fatalf("rahasia bocor ke argumen/lingkungan unit:\n%s", all)
	}
	res, _ = c.ListOp()(context.Background(), nil)
	if raw, _ := json.Marshal(res); strings.Contains(string(raw), "rahasia") || strings.Contains(string(raw), added.Password) {
		t.Fatalf("cloud.list membocorkan rahasia: %s", raw)
	}
	// Repo yang sudah ada di tempat itu.
	fr.fail["init"] = errors.New("Fatal: create key in repository at s3:... failed: repository master key and config already initialized")
	if _, err := cloudCall(t, c.RepoAddOp(), repo); !errors.As(err, &oe) || oe.Code != "cloud_repo_exists" {
		t.Fatalf("repo sudah ada: %v", err)
	}
	delete(fr.fail, "init")
	// Sambung repo yang ada: password salah.
	ex := repo
	ex.Existing, ex.Password = true, "password-salah"
	fr.fail["cat"] = errors.New("Fatal: wrong password or no key found")
	if _, err := cloudCall(t, c.RepoAddOp(), ex); !errors.As(err, &oe) || oe.Code != "cloud_bad_password" {
		t.Fatalf("password salah: %v", err)
	}
	if ents, _ := os.ReadDir(c.t.SecretsDir); len(ents) != 2 {
		t.Fatalf("rahasia repo yang gagal harus dibuang: %v", ents)
	}

	// Tugas dan backup.
	res, err = cloudCall(t, c.JobSetOp(), proto.CloudJob{Repo: added.ID, Pool: "tank", Share: "keluarga", Hour: 3, KeepDays: 30, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	id := res.(map[string]string)["id"]
	if id != "keluarga-c"+added.ID[:6] {
		t.Fatalf("id = %q", id)
	}
	_ = os.MkdirAll(filepath.Join(b.shares.t.MountBase, "tank/keluarga/.zfs/snapshot", cloudSnapName(id)), 0o755)
	fr.out["backup"] = `{"message_type":"summary","data_added":1234,"total_bytes_processed":5678}`
	if err := c.execute(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	bk := strings.Join(fr.find("backup"), " ")
	for _, want := range []string{"--property=CapabilityBoundingSet=CAP_DAC_READ_SEARCH", "--property=AmbientCapabilities=CAP_DAC_READ_SEARCH", "--tag job:" + id, "--tag share:keluarga",
		"-- " + filepath.Join(b.shares.t.MountBase, "tank/keluarga/.zfs/snapshot", cloudSnapName(id))} {
		if !strings.Contains(bk, want) {
			t.Errorf("backup tidak memuat %q:\n%s", want, bk)
		}
	}
	if fg := strings.Join(fr.find("forget"), " "); !strings.Contains(fg, "--group-by host,tags --keep-within 30d --keep-daily 7 --keep-last 1 --prune") || !strings.Contains(fg, "--tag job:"+id) {
		t.Fatalf("forget: %s", fg)
	}
	if _, ok := f.snaps["tank/keluarga@"+cloudSnapName(id)]; ok {
		t.Fatal("snapshot sementara harus dibuang")
	}
	res, _ = c.ListOp()(context.Background(), nil)
	if j := res.(proto.CloudListResult).Jobs[0]; !j.LastOK || j.LastAdded != 1234 {
		t.Fatalf("hasil tugas = %+v", j)
	}
	// Folder yang di-backup ke cloud tidak bisa dihapus; repo yang dipakai juga.
	if id := c.usingShare("tank", "keluarga"); id == "" {
		t.Fatal("usingShare")
	}
	if _, err := cloudCall(t, c.RepoDeleteOp(), proto.CloudIDParams{ID: added.ID}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("hapus repo yang dipakai: %v", err)
	}
	// Gagal: tercatat.
	fr.fail["backup"] = errors.New("Fatal: unable to open repository: Access Denied")
	if err := c.execute(context.Background(), id); !errors.As(err, &oe) || oe.Code != "cloud_denied" {
		t.Fatalf("backup ditolak S3: %v", err)
	}
}

func TestCloudRestore(t *testing.T) {
	c, fr, f, b := testCloud(t)
	res, err := cloudCall(t, c.RepoAddOp(), proto.CloudRepoParams{Name: "MinIO", Endpoint: "10.0.2.2:9000", Bucket: "uji", InsecureHTTP: true, AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	repo := res.(proto.CloudRepoAdded).ID
	if init := strings.Join(fr.find("init"), " "); !strings.Contains(init, "--repo s3:http://10.0.2.2:9000/uji") {
		t.Fatalf("http: %s", init)
	}
	fr.out["snapshots"] = `[{"id":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","time":"2026-10-28T03:00:00Z","hostname":"nas-lama","paths":["/srv/sapo/tank/keluarga/.zfs/snapshot/sapo-cloud-x"],"tags":["sapo","job:x","share:keluarga"]},
	{"id":"zz","paths":["/x"]}]`
	var oe *OpError
	if _, err := cloudCall(t, c.RestoreOp(), proto.CloudRestoreParams{Repo: repo, Snapshot: "0123456789abcdef", Pool: "tank", Name: "keluarga"}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("folder yang ada: %v", err)
	}
	if _, err := cloudCall(t, c.RestoreOp(), proto.CloudRestoreParams{Repo: repo, Snapshot: "--target=/etc", Pool: "tank", Name: "pulih"}); !errors.As(err, &oe) {
		t.Fatalf("id salinan aneh: %v", err)
	}
	// Dataset baru "terpasang" di tempatnya.
	mp := filepath.Join(b.shares.t.MountBase, "tank/pulih")
	orig := b.propsFn
	b.propsFn = func(ctx context.Context, ds string, names ...string) (map[string]string, error) {
		if ds == "tank/pulih" {
			if _, ok := f.ds[ds]; ok {
				return map[string]string{"mounted": "yes", "mountpoint": mp, "type": "filesystem", sharePropName: "pulih"}, nil
			}
		}
		return orig(ctx, ds, names...)
	}
	if _, err := cloudCall(t, c.RestoreOp(), proto.CloudRestoreParams{Repo: repo, Snapshot: "0123456789abcdef", Pool: "tank", Name: "pulih"}); err != nil {
		t.Fatal(err)
	}
	b.runMu.Lock()
	b.runMu.Unlock()
	rs := strings.Join(fr.find("restore"), " ")
	for _, want := range []string{"--property=ReadWritePaths=" + mp + " ", "--property=AmbientCapabilities=CAP_CHOWN CAP_FOWNER CAP_DAC_OVERRIDE",
		"restore 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef:/srv/sapo/tank/keluarga/.zfs/snapshot/sapo-cloud-x --target " + mp} {
		if !strings.Contains(rs, want) {
			t.Errorf("restore tidak memuat %q:\n%s", want, rs)
		}
	}
	if p := f.ds["tank/pulih"]; p == nil || p[sharePropName] != "pulih" {
		t.Fatalf("folder baru = %+v", p)
	}
	st, _ := b.loadLocked()
	if r := st.Restores[len(st.Restores)-1]; !r.OK || r.Name != "pulih" || r.JobID != "cloud:"+repo {
		t.Fatalf("catatan pemulihan = %+v", r)
	}
	// Gagal: folder baru buatan pemulihan dibuang lagi.
	fr.fail["restore"] = errors.New("Fatal: network")
	if _, err := cloudCall(t, c.RestoreOp(), proto.CloudRestoreParams{Repo: repo, Snapshot: "0123456789abcdef", Pool: "tank", Name: "pulih2"}); err != nil {
		t.Fatal(err)
	}
	b.runMu.Lock()
	b.runMu.Unlock()
	if !strings.Contains(strings.Join(f.calls, "\n"), "destroy -r tank/pulih2") {
		t.Fatalf("folder gagal harus dibuang:\n%s", strings.Join(f.calls, "\n"))
	}
}

// Jam belum tersinkron: backup tetap jalan, pemangkasan ditunda.
func TestCloudSkipsPruneWithoutClock(t *testing.T) {
	c, fr, _, b := testCloud(t)
	c.clockOK = func(context.Context) bool { return false }
	res, err := cloudCall(t, c.RepoAddOp(), proto.CloudRepoParams{Name: "x", Endpoint: "s3.example.com", Bucket: "abc", AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	repo := res.(proto.CloudRepoAdded).ID
	res, err = cloudCall(t, c.JobSetOp(), proto.CloudJob{Repo: repo, Pool: "tank", Share: "keluarga", Hour: 3, KeepDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	id := res.(map[string]string)["id"]
	_ = os.MkdirAll(filepath.Join(b.shares.t.MountBase, "tank/keluarga/.zfs/snapshot", cloudSnapName(id)), 0o755)
	if err := c.execute(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if fr.find("backup") == nil || fr.find("forget") != nil {
		t.Fatal("backup harus jalan, pemangkasan harus ditunda")
	}
}

func TestResetOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("butuh root (chown)")
	}
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "a/b"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "a/b/f"), []byte("x"), 0o4755)
	_ = os.Lchown(filepath.Join(dir, "a/b/f"), 1001, 1001)
	_ = os.Symlink("/etc/shadow", filepath.Join(dir, "a/l"))
	if err := resetOwnership(dir); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Lstat(filepath.Join(dir, "a/b/f"))
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || sys.Uid != 0 || sys.Gid != 0 || st.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("pemilik/mode tidak di-reset: %v %+v", st.Mode(), st.Sys())
	}
	// Symlink tidak diikuti: /etc/shadow tidak tersentuh.
	if lt, _ := os.Lstat(filepath.Join(dir, "a/l")); lt.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink harus tetap symlink")
	}
}

const testSnapJSON = `[{"id":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","time":"2026-10-28T03:00:00Z","hostname":"nas-lama","paths":["/srv/sapo/tank/keluarga/.zfs/snapshot/sapo-cloud-x"],"tags":["sapo","job:x","share:keluarga"]}]`

func TestCloudLs(t *testing.T) {
	c, fr, _, _ := testCloud(t)
	c.t.ListMax = 2
	res, err := cloudCall(t, c.RepoAddOp(), proto.CloudRepoParams{Name: "x", Endpoint: "s3.example.com", Bucket: "abc", AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	repo := res.(proto.CloudRepoAdded).ID
	fr.out["snapshots"] = testSnapJSON
	base := "/srv/sapo/tank/keluarga/.zfs/snapshot/sapo-cloud-x"
	fr.out["ls"] = `{"time":"2026-10-28T03:00:00Z","paths":["` + base + `"],"id":"0123","struct_type":"snapshot"}
{"name":"foto","type":"dir","path":"` + base + `/foto","size":4096,"struct_type":"node"}
{"name":"foto","type":"dir","path":"` + base + `/foto/foto","struct_type":"node"}
{"name":"b.txt","type":"file","path":"` + base + `/foto/b.txt","size":5,"mtime":"2026-10-27T10:00:00+07:00","struct_type":"node"}
{"name":"a","type":"dir","path":"` + base + `/foto/a","struct_type":"node"}
{"name":"palsu","type":"file","path":"` + base + `/lain/palsu","struct_type":"node"}
{"name":"z","type":"fifo","path":"` + base + `/foto/z","struct_type":"node"}
potongan`
	var oe *OpError
	for _, bad := range []string{"../etc", "foto/../..", "foto//a", "a\x00b"} {
		if _, err := cloudCall(t, c.LsOp(), proto.CloudLsParams{Repo: repo, Snapshot: "01234567", Path: bad}); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
			t.Errorf("path %q harus ditolak: %v", bad, err)
		}
	}
	out, err := cloudCall(t, c.LsOp(), proto.CloudLsParams{Repo: repo, Snapshot: "01234567", Path: "/foto/"})
	if err != nil {
		t.Fatal(err)
	}
	ls := out.(proto.CloudLsResult)
	// Hanya anak langsung foto/, dibatasi ListMax (urutan restic), folder dulu.
	if len(ls.Entries) != 2 || !ls.Truncated || ls.Entries[0].Name != "foto" || ls.Entries[1].Name != "b.txt" || ls.Entries[0].Type != "dir" {
		t.Fatalf("isi = %+v", ls)
	}
	if argv := strings.Join(fr.find("ls"), " "); !strings.Contains(argv, "ls --json 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef "+base+"/foto") ||
		!strings.Contains(argv, "--property=CapabilityBoundingSet= ") {
		t.Fatalf("argv ls: %s", argv)
	}
	c.t.ListMax = 10
	out, _ = cloudCall(t, c.LsOp(), proto.CloudLsParams{Repo: repo, Snapshot: "01234567", Path: "foto"})
	if ls := out.(proto.CloudLsResult); len(ls.Entries) != 4 || ls.Truncated || ls.Entries[2].Name != "b.txt" || ls.Entries[2].Size != 5 || ls.Entries[3].Type != "other" {
		t.Fatalf("isi = %+v", ls)
	}
	// Daftar salinan diambil sekali (cache).
	n := 0
	for _, a := range fr.calls {
		if hasSub(a, "snapshots") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("daftar salinan diminta %d kali", n)
	}
}

func TestCloudRestorePath(t *testing.T) {
	c, fr, f, b := testCloud(t)
	res, err := cloudCall(t, c.RepoAddOp(), proto.CloudRepoParams{Name: "x", Endpoint: "s3.example.com", Bucket: "abc", AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	repo := res.(proto.CloudRepoAdded).ID
	fr.out["snapshots"] = testSnapJSON
	mp := filepath.Join(b.shares.t.MountBase, "tank/pulih")
	orig := b.propsFn
	b.propsFn = func(ctx context.Context, ds string, names ...string) (map[string]string, error) {
		if _, ok := f.ds[ds]; ok && strings.HasPrefix(ds, "tank/pulih") {
			return map[string]string{"mounted": "yes", "mountpoint": filepath.Join(b.shares.t.MountBase, ds), "type": "filesystem"}, nil
		}
		return orig(ctx, ds, names...)
	}
	var oe *OpError
	if _, err := cloudCall(t, c.RestoreOp(), proto.CloudRestoreParams{Repo: repo, Snapshot: "01234567", Pool: "tank", Name: "pulih", Path: "../x"}); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
		t.Fatalf("path keluar salinan: %v", err)
	}
	// "restic" memulihkan file yang diminta.
	_ = os.MkdirAll(filepath.Join(mp, "foto [2026]"), 0o755)
	_ = os.WriteFile(filepath.Join(mp, "foto [2026]/a*.jpg"), []byte("x"), 0o644)
	if _, err := cloudCall(t, c.RestoreOp(), proto.CloudRestoreParams{Repo: repo, Snapshot: "01234567", Pool: "tank", Name: "pulih", Path: "foto [2026]/a*.jpg"}); err != nil {
		t.Fatal(err)
	}
	b.runMu.Lock()
	b.runMu.Unlock()
	argv := fr.find("restore")
	if got := argv[len(argv)-2:]; got[0] != "--include" || got[1] != `/foto \[2026\]/a\*.jpg` {
		t.Fatalf("include = %q", got)
	}
	st, _ := b.loadLocked()
	if r := st.Restores[len(st.Restores)-1]; !r.OK {
		t.Fatalf("pemulihan = %+v", r)
	}
	// Tidak ada di salinan: folder baru dibuang, pemulihan gagal.
	if _, err := cloudCall(t, c.RestoreOp(), proto.CloudRestoreParams{Repo: repo, Snapshot: "01234567", Pool: "tank", Name: "pulih2", Path: "tidak-ada.txt"}); err != nil {
		t.Fatal(err)
	}
	b.runMu.Lock()
	b.runMu.Unlock()
	st, _ = b.loadLocked()
	if r := st.Restores[len(st.Restores)-1]; r.OK || r.Name != "pulih2" || !strings.Contains(strings.Join(f.calls, "\n"), "destroy -r tank/pulih2") {
		t.Fatalf("pemulihan file yang tidak ada = %+v", r)
	}
}
