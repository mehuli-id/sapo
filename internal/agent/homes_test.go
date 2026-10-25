package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

func testHomes(t *testing.T) (*Homes, string, string) {
	t.Helper()
	s, root, log := testShares(t, "0")
	// Pemilik direkam lewat chown tiruan (test tidak berjalan sebagai root).
	owners := map[string]int{}
	s.chown = func(p string, uid, gid int) error {
		owners[p] = uid
		return nil
	}
	s.isMount = func(string) bool { return true }
	managed := func(name string) (int, int, bool) {
		switch name {
		case "ani":
			return 20011, 20000, true
		case "budi":
			return 20012, 20000, true
		}
		return 0, 0, false
	}
	h := NewHomes(s, managed)
	h.owner = func(p string) (int, error) { return owners[p], nil }
	return h, root, log
}

func homesCall(t *testing.T, h Handler, p any) (any, error) {
	t.Helper()
	raw, _ := json.Marshal(p)
	return h(context.Background(), raw)
}

func TestHomesApply(t *testing.T) {
	h, root, log := testHomes(t)
	res, err := homesCall(t, h.ApplyOp(), proto.HomesApplyParams{Pool: "tank", Users: []proto.HomeUser{
		{Name: "ani", QuotaBytes: 10 << 30}, {Name: "budi"}, {Name: "hantu"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Akun yang bukan akun Sapo tidak mendapat folder; sisanya tetap dibuat.
	if f := res.(proto.HomesApplyResult).Failures; len(f) != 1 || !strings.HasPrefix(f[0], "hantu:") {
		t.Fatalf("kegagalan = %v", f)
	}
	calls, _ := os.ReadFile(log)
	inOrder(t, string(calls),
		"zfs create -o sapo:home=@induk -o devices=off -o setuid=off -- tank/sapo-home",
		"zfs create -o sapo:home=ani -o devices=off -o setuid=off -- tank/sapo-home/ani",
		"zfs set refquota=10737418240 -- tank/sapo-home/ani",
		"zfs create -o sapo:home=budi -o devices=off -o setuid=off -- tank/sapo-home/budi")
	if strings.Contains(string(calls), "refquota=none -- tank/sapo-home/budi") || strings.Contains(string(calls), "tank/sapo-home/hantu") {
		t.Fatalf("panggilan salah:\n%s", calls)
	}
	for p, mode := range map[string]os.FileMode{"tank/sapo-home": 0o711, "tank/sapo-home/ani": 0o700} {
		if fi, err := os.Stat(filepath.Join(root, p)); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("mode %s = %v (%v)", p, fi.Mode().Perm(), err)
		}
	}
	// Kuota dicabut: diset ke none.
	_ = os.Truncate(log, 0)
	if _, err := homesCall(t, h.ApplyOp(), proto.HomesApplyParams{Pool: "tank", Users: []proto.HomeUser{{Name: "ani"}}}); err != nil {
		t.Fatal(err)
	}
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "zfs set refquota=none -- tank/sapo-home/ani") {
		t.Fatalf("kuota dicabut:\n%s", calls)
	}
	// Nama tidak valid atau akun aplikasi: ditolak seluruhnya.
	var oe *OpError
	for _, u := range []string{"../root", "sapo-app-homer"} {
		if _, err := homesCall(t, h.ApplyOp(), proto.HomesApplyParams{Pool: "tank", Users: []proto.HomeUser{{Name: u}}}); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
			t.Errorf("%s harus ditolak: %v", u, err)
		}
	}
}

func TestHomesRefuseForeignDataset(t *testing.T) {
	h, root, _ := testHomes(t)
	// tank/sapo-home dibuat dari luar Sapo (tanpa penanda): tidak dipakai.
	_ = os.MkdirAll(filepath.Join(root, "tank/sapo-home"), 0o755)
	var oe *OpError
	if _, err := homesCall(t, h.ApplyOp(), proto.HomesApplyParams{Pool: "tank", Users: []proto.HomeUser{{Name: "ani"}}}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("dataset asing: %v", err)
	}
}

func TestHomesDeleteRefusesExistingUser(t *testing.T) {
	h, _, _ := testHomes(t)
	if _, err := homesCall(t, h.ApplyOp(), proto.HomesApplyParams{Pool: "tank", Users: []proto.HomeUser{{Name: "ani"}}}); err != nil {
		t.Fatal(err)
	}
	var oe *OpError
	if _, err := homesCall(t, h.DeleteOp(), proto.HomeDeleteParams{Pool: "tank", User: "ani"}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("folder pengguna yang masih ada: %v", err)
	}
	if _, err := homesCall(t, h.DeleteOp(), proto.HomeDeleteParams{Pool: "tank", User: "cici"}); !errors.As(err, &oe) || oe.Code != proto.ErrNotFound {
		t.Fatalf("folder yang tidak ada: %v", err)
	}
}

func TestSMBConfHomes(t *testing.T) {
	s, root, _ := testShares(t, "0")
	conf := string(s.renderSMBConf(nil, "tank", nil))
	for _, w := range []string{"[homes]\n", "   path = " + filepath.Join(root, "tank/sapo-home") + "/%S\n", "   valid users = %S\n", "   browseable = no\n"} {
		if !strings.Contains(conf, w) {
			t.Errorf("smb.conf tanpa %q:\n%s", w, conf)
		}
	}
	if strings.Contains(string(s.renderSMBConf(nil, "", nil)), "[homes]") {
		t.Fatal("tanpa pool folder pribadi, [homes] tidak boleh ada")
	}
	// SMB dimatikan: folder pribadi juga tidak dibagikan.
	raw, _ := json.Marshal(proto.SharesApplyParams{Shares: []proto.ShareSpec{}, SMBOff: true, HomesPool: "tank"})
	if _, err := s.ApplyOp()(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(s.t.SMBConf); strings.Contains(string(b), "[homes]") {
		t.Fatalf("SMB mati tetapi [homes] ada:\n%s", b)
	}
}

// Dataset sapo-apps buatan luar Sapo (penanda tidak diset terbaca "-")
// tidak dipakai App Center.
func TestAppsRefuseForeignParent(t *testing.T) {
	a, root, _ := testApps(t)
	_ = os.MkdirAll(filepath.Join(root, "tank/sapo-apps"), 0o755)
	var oe *OpError
	if _, err := appCall(t, a.InstallOp(), proto.AppInstallParams{ID: "homer", Pool: "tank", Port: 8090}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("dataset sapo-apps asing: %v", err)
	}
}

// Folder sisa pengguna lama (pemilik UID lain) tidak diserahkan ke akun baru
// bernama sama, misalnya setelah pool lama diimpor lagi.
func TestHomesRefuseForeignOwner(t *testing.T) {
	h, root, log := testHomes(t)
	if _, err := homesCall(t, h.ApplyOp(), proto.HomesApplyParams{Pool: "tank", Users: []proto.HomeUser{{Name: "ani"}}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tank/sapo-home/ani")
	h.owner = func(p string) (int, error) {
		if p == path {
			return 20999, nil
		}
		return 0, nil
	}
	_ = os.Truncate(log, 0)
	res, err := homesCall(t, h.ApplyOp(), proto.HomesApplyParams{Pool: "tank", Users: []proto.HomeUser{{Name: "ani"}}})
	if err != nil {
		t.Fatal(err)
	}
	if f := res.(proto.HomesApplyResult).Failures; len(f) != 1 || !strings.Contains(f[0], "pemilik folder tidak cocok") {
		t.Fatalf("kegagalan = %v", f)
	}
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "zfs set") {
		t.Fatalf("folder asing tidak boleh diubah:\n%s", calls)
	}
}

func TestHomesDeleteRefusesPasswdEntry(t *testing.T) {
	h, _, _ := testHomes(t)
	// ani ada di /etc/passwd (testShares) walau tidak tercatat sebagai akun
	// Sapo: ditolak.
	h.managed = func(string) (int, int, bool) { return 0, 0, false }
	var oe *OpError
	if _, err := homesCall(t, h.DeleteOp(), proto.HomeDeleteParams{Pool: "tank", User: "ani"}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("akun Linux masih ada: %v", err)
	}
}

// Dataset sapo-apps dari App Center tahap 1 (penanda "-" diset langsung)
// dipindah ke penanda baru.
func TestAppsMigrateOldParentMarker(t *testing.T) {
	a, root, log := testApps(t)
	_ = os.MkdirAll(filepath.Join(root, "tank/sapo-apps"), 0o755)
	props := filepath.Join(filepath.Dir(log), "props")
	_ = os.WriteFile(filepath.Join(props, "tank_sapo-apps.sapo_app"), []byte("-\n"), 0o644)
	if _, err := appCall(t, a.InstallOp(), proto.AppInstallParams{ID: "homer", Pool: "tank", Port: 8090}); err != nil {
		t.Fatal(err)
	}
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "zfs set sapo:app=@induk -- tank/sapo-apps") {
		t.Fatalf("penanda lama tidak dipindah:\n%s", calls)
	}
}
