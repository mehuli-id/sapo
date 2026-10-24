package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// testAccounts menyiapkan salinan passwd/group dan program pengganti yang
// mengubahnya (testdata/accounts/fake.sh).
func testAccounts(t *testing.T) (*Accounts, string) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"passwd", "group", "fake.sh"} {
		b, err := os.ReadFile(filepath.Join("testdata", "accounts", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tool := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.Symlink(filepath.Join(dir, "fake.sh"), p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return NewAccounts(AccountTools{
		Useradd: tool("useradd"), Userdel: tool("userdel"), Groupadd: tool("groupadd"), Groupdel: tool("groupdel"),
		Gpasswd: tool("gpasswd"), Smbpasswd: tool("smbpasswd"),
		PasswdFile: filepath.Join(dir, "passwd"), GroupFile: filepath.Join(dir, "group"),
		IDMin: 20000, IDMax: 29999, PrimaryGroup: "sapo-users", Shell: "/usr/sbin/nologin", Timeout: 5 * time.Second,
	}), dir
}

func syncAccounts(t *testing.T, a *Accounts, p proto.AccountsSyncParams) (proto.AccountsSyncResult, error) {
	t.Helper()
	raw, _ := json.Marshal(p)
	res, err := a.SyncOp()(context.Background(), raw)
	if err != nil {
		return proto.AccountsSyncResult{}, err
	}
	return res.(proto.AccountsSyncResult), nil
}

func TestAccountsSync(t *testing.T) {
	a, dir := testAccounts(t)
	res, err := syncAccounts(t, a, proto.AccountsSyncParams{
		Users: []proto.AccountUser{
			{Name: "budi", Groups: []string{"keluarga"}},
			{Name: "ani", Groups: []string{"keluarga"}, Disabled: true},
			{Name: "admin"}, // akun sistem lain dengan nama sama: tidak diambil alih
		},
		Groups: []string{"keluarga", "sudo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(res.CreatedUsers)
	if !slices.Equal(res.CreatedUsers, []string{"ani", "budi"}) || !slices.Equal(res.CreatedGroups, []string{"keluarga"}) {
		t.Fatalf("hasil = %+v", res)
	}
	slices.Sort(res.Conflicts)
	if !slices.Equal(res.Conflicts, []string{"group:sudo", "user:admin"}) {
		t.Fatalf("bentrok = %v", res.Conflicts)
	}
	// Akun dan grup Sapo lama yang tidak ada lagi dihapus; akun di luar
	// rentang atau berkelompok utama lain (asing) tidak disentuh.
	slices.Sort(res.RemovedUsers)
	if !slices.Equal(res.RemovedUsers, []string{"lama"}) || !slices.Equal(res.RemovedGroups, []string{"bekas"}) {
		t.Fatalf("dihapus = %+v", res)
	}
	users, _ := readPasswd(filepath.Join(dir, "passwd"))
	groups, _ := readGroup(filepath.Join(dir, "group"))
	for _, keep := range []string{"root", "daemon", "sshd", "admin", "asing"} {
		if _, ok := users[keep]; !ok {
			t.Errorf("akun %s tidak boleh hilang", keep)
		}
	}
	if b := users["budi"]; b.UID < 20000 || b.GID != 20000 {
		t.Fatalf("budi = %+v", b)
	}
	if m := groups["keluarga"].Members; !slices.Equal(m, []string{"ani", "budi"}) {
		t.Fatalf("anggota keluarga = %v", m)
	}
	if !slices.Equal(groups["sudo"].Members, []string{"admin"}) {
		t.Fatalf("grup sudo tidak boleh berubah: %v", groups["sudo"].Members)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
	if !strings.Contains(string(calls), "smbpasswd -d -- ani") || !strings.Contains(string(calls), "smbpasswd -e -- budi") {
		t.Fatalf("status Samba tidak disinkronkan:\n%s", calls)
	}

	// Diulang tanpa perubahan: tidak ada akun dibuat atau dihapus.
	res, err = syncAccounts(t, a, proto.AccountsSyncParams{
		Users:  []proto.AccountUser{{Name: "budi", Groups: []string{"keluarga"}}, {Name: "ani", Groups: []string{"keluarga"}}},
		Groups: []string{"keluarga"},
	})
	if err != nil || len(res.CreatedUsers)+len(res.RemovedUsers)+len(res.CreatedGroups)+len(res.RemovedGroups) != 0 {
		t.Fatalf("sinkron ulang: %+v %v", res, err)
	}
	// budi keluar dari grup.
	if _, err := syncAccounts(t, a, proto.AccountsSyncParams{
		Users:  []proto.AccountUser{{Name: "budi"}, {Name: "ani", Groups: []string{"keluarga"}}},
		Groups: []string{"keluarga"},
	}); err != nil {
		t.Fatal(err)
	}
	groups, _ = readGroup(filepath.Join(dir, "group"))
	if m := groups["keluarga"].Members; !slices.Equal(m, []string{"ani"}) {
		t.Fatalf("anggota keluarga = %v", m)
	}
}

func TestAccountsSyncRejects(t *testing.T) {
	a, dir := testAccounts(t)
	var oe *OpError
	for name, p := range map[string]proto.AccountsSyncParams{
		"nama diawali tanda hubung": {Users: []proto.AccountUser{{Name: "-o"}}},
		"huruf besar":               {Users: []proto.AccountUser{{Name: "Budi"}}},
		"grup tidak terdaftar":      {Users: []proto.AccountUser{{Name: "budi", Groups: []string{"lain"}}}},
		"grup utama":                {Groups: []string{"sapo-users"}},
		"nama pengguna = grup":      {Users: []proto.AccountUser{{Name: "keluarga"}}, Groups: []string{"keluarga"}},
		"ganda":                     {Users: []proto.AccountUser{{Name: "budi"}, {Name: "budi"}}},
	} {
		if _, err := syncAccounts(t, a, p); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "calls.log")); err == nil {
		t.Fatal("tidak boleh ada program yang dijalankan")
	}
}

func TestSMBPassword(t *testing.T) {
	a, dir := testAccounts(t)
	if _, err := syncAccounts(t, a, proto.AccountsSyncParams{Users: []proto.AccountUser{{Name: "budi"}}}); err != nil {
		t.Fatal(err)
	}
	set := func(name, pw string) error {
		raw, _ := json.Marshal(proto.SMBPasswordParams{Name: name, Password: pw})
		_, err := a.SMBPasswordOp()(context.Background(), raw)
		return err
	}
	if err := set("budi", "padi menguning"); err != nil {
		t.Fatal(err)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "smbpasswd.stdin"))
	if string(stdin) != "padi menguning\npadi menguning\n" {
		t.Fatalf("stdin = %q", stdin)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
	if strings.Contains(string(calls), "padi") {
		t.Fatal("password tidak boleh ada di argumen program")
	}
	var oe *OpError
	// Akun di luar Sapo (admin lokal, root) dan password berbaris baru ditolak.
	for _, tc := range [][2]string{{"admin", "x"}, {"root", "x"}, {"budi", "a\nb"}, {"budi", ""}} {
		if err := set(tc[0], tc[1]); !errors.As(err, &oe) {
			t.Errorf("%q harus ditolak: %v", tc, err)
		}
	}
}
