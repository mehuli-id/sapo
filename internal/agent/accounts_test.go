package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	for _, f := range []string{"passwd", "group", "fake.sh", "state.json"} {
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
	// /proc tiruan: proses 4242 (smbd, sesi "ani" menurut smbstatus) dan 5001
	// (berjalan dengan UID 20001 milik akun lama).
	proc := filepath.Join(dir, "proc")
	for pid, f := range map[string][2]string{
		"4242": {"smbd", "Uid:\t0\t0\t0\t0\n"},
		"5001": {"smbd", "Uid:\t20001\t20001\t20001\t20001\n"},
		"6000": {"bash", "Uid:\t1000\t1000\t1000\t1000\n"},
	} {
		_ = os.MkdirAll(filepath.Join(proc, pid), 0o755)
		_ = os.WriteFile(filepath.Join(proc, pid, "comm"), []byte(f[0]+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(proc, pid, "status"), []byte("Name:\t"+f[0]+"\n"+f[1]), 0o644)
	}
	smbstatus := filepath.Join(dir, "smbstatus")
	_ = os.WriteFile(smbstatus, []byte("#!/bin/sh\nprintf '%s\\n' '{\"sessions\":{\"1\":{\"username\":\"SAPO\\\\ani\",\"server_id\":{\"pid\":\"4242\"}}}}'\n"), 0o755)
	a := NewAccounts(AccountTools{
		Useradd: tool("useradd"), Userdel: tool("userdel"), Groupadd: tool("groupadd"), Groupdel: tool("groupdel"),
		Gpasswd: tool("gpasswd"), Smbpasswd: tool("smbpasswd"), Smbstatus: smbstatus, ProcDir: proc,
		PasswdFile: filepath.Join(dir, "passwd"), GroupFile: filepath.Join(dir, "group"), StateFile: filepath.Join(dir, "state.json"),
		IDMin: 20000, IDMax: 29999, PrimaryGroup: "sapo-users", Shell: "/usr/sbin/nologin", Timeout: 5 * time.Second,
	})
	a.kill = func(pid int) error {
		f, _ := os.OpenFile(filepath.Join(dir, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		defer f.Close()
		fmt.Fprintf(f, "kill %d\n", pid)
		return nil
	}
	return a, dir
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
	// Sesi akun yang dihapus (lama: proses ber-UID 20001) dan yang
	// dinonaktifkan (ani: sesi smbd dari smbstatus) diputus; proses lain tidak.
	if !strings.Contains(string(calls), "kill 5001") || !strings.Contains(string(calls), "kill 4242") || strings.Contains(string(calls), "kill 6000") {
		t.Fatalf("sesi tidak diputus dengan benar:\n%s", calls)
	}

	// ID baru dimulai dari catatan (tidak memakai ulang ID akun lama).
	if users["budi"].UID < 20011 || users["ani"].UID < 20011 {
		t.Fatalf("ID baru = %d %d", users["budi"].UID, users["ani"].UID)
	}
	os.Remove(filepath.Join(dir, "calls.log"))
	// Diulang tanpa perubahan: tidak ada akun dibuat atau dihapus, dan
	// status Samba yang sudah diterapkan tidak diulang.
	res, err = syncAccounts(t, a, proto.AccountsSyncParams{
		Users:  []proto.AccountUser{{Name: "budi", Groups: []string{"keluarga"}}, {Name: "ani", Groups: []string{"keluarga"}}},
		Groups: []string{"keluarga"},
	})
	if err != nil || len(res.CreatedUsers)+len(res.RemovedUsers)+len(res.CreatedGroups)+len(res.RemovedGroups) != 0 {
		t.Fatalf("sinkron ulang: %+v %v", res, err)
	}
	if calls, _ := os.ReadFile(filepath.Join(dir, "calls.log")); strings.Contains(string(calls), "smbpasswd -e -- budi") {
		t.Fatalf("status Samba yang sama tidak perlu diterapkan ulang:\n%s", calls)
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
	for _, tc := range [][2]string{{"admin", "x"}, {"root", "x"}, {"budi", "a\nb"}, {"budi", ""}, {"budi", strings.Repeat("x", 128)}, {"budi", "a\tb"}} {
		if err := set(tc[0], tc[1]); !errors.As(err, &oe) {
			t.Errorf("%q harus ditolak: %v", tc, err)
		}
	}
}

func TestAccountsNeverReuseIDs(t *testing.T) {
	a, dir := testAccounts(t)
	if _, err := syncAccounts(t, a, proto.AccountsSyncParams{Users: []proto.AccountUser{{Name: "budi"}}}); err != nil {
		t.Fatal(err)
	}
	users, _ := readPasswd(filepath.Join(dir, "passwd"))
	old := users["budi"].UID
	// budi dihapus, lalu carol dibuat: carol tidak boleh mendapat UID budi.
	if _, err := syncAccounts(t, a, proto.AccountsSyncParams{Users: []proto.AccountUser{{Name: "carol"}}}); err != nil {
		t.Fatal(err)
	}
	users, _ = readPasswd(filepath.Join(dir, "passwd"))
	if _, ok := users["budi"]; ok || users["carol"].UID <= old {
		t.Fatalf("budi %d, carol %d", old, users["carol"].UID)
	}
}

func TestAccountsRefuseForeignPrimaryGroup(t *testing.T) {
	a, dir := testAccounts(t)
	// Grup sapo-users yang tidak tercatat sebagai buatan sapod.
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"next_id":20011}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var oe *OpError
	if _, err := syncAccounts(t, a, proto.AccountsSyncParams{Users: []proto.AccountUser{{Name: "budi"}}}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls.log")); err == nil {
		t.Fatal("tidak boleh ada perubahan")
	}
}

func TestAccountsDisableFailureIsRetried(t *testing.T) {
	a, dir := testAccounts(t)
	p := proto.AccountsSyncParams{Users: []proto.AccountUser{{Name: "ani", Disabled: true}}}
	// smbpasswd -d gagal sementara (bukan karena akun belum ada di Samba).
	smb := filepath.Join(dir, "smbpasswd")
	os.Remove(smb)
	_ = os.WriteFile(smb, []byte("#!/bin/sh\necho 'tdb terkunci' >&2\nexit 1\n"), 0o755)
	res, err := syncAccounts(t, a, p)
	if err != nil || len(res.Failures) != 1 || !strings.HasPrefix(res.Failures[0], "user:ani") {
		t.Fatalf("hasil = %+v %v", res, err)
	}
	// Status tidak dicatat, jadi dicoba lagi pada sinkronisasi berikutnya.
	_ = os.WriteFile(smb, []byte("#!/bin/sh\necho \"$*\" >> '"+filepath.Join(dir, "smb.log")+"'\n"), 0o755)
	if _, err := syncAccounts(t, a, p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "smb.log")); !strings.Contains(string(b), "-d -- ani") {
		t.Fatalf("nonaktif tidak dicoba ulang: %q", b)
	}
	// Akun yang belum ada di Samba: -d gagal "Failed to find entry" = wajar.
	_ = os.WriteFile(smb, []byte("#!/bin/sh\necho 'Failed to find entry for user budi.' >&2\nexit 1\n"), 0o755)
	res, err = syncAccounts(t, a, proto.AccountsSyncParams{Users: []proto.AccountUser{{Name: "ani", Disabled: true}, {Name: "budi", Disabled: true}}})
	if err != nil || len(res.Failures) != 0 {
		t.Fatalf("akun tanpa entri Samba: %+v %v", res, err)
	}
}
