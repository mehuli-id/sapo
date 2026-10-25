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
	var chowned []string
	s.chown = func(p string, uid, gid int) error {
		chowned = append(chowned, p)
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
	return NewHomes(s, managed), root, log
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
		"zfs set quota=10737418240 -- tank/sapo-home/ani",
		"zfs create -o sapo:home=budi -o devices=off -o setuid=off -- tank/sapo-home/budi")
	if strings.Contains(string(calls), "quota=none -- tank/sapo-home/budi") || strings.Contains(string(calls), "tank/sapo-home/hantu") {
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
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "zfs set quota=none -- tank/sapo-home/ani") {
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
	conf := string(s.renderSMBConf(nil, "tank"))
	for _, w := range []string{"[homes]\n", "   path = " + filepath.Join(root, "tank/sapo-home") + "/%S\n", "   valid users = %S\n", "   browseable = no\n"} {
		if !strings.Contains(conf, w) {
			t.Errorf("smb.conf tanpa %q:\n%s", w, conf)
		}
	}
	if strings.Contains(string(s.renderSMBConf(nil, "")), "[homes]") {
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
