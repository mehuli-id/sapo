package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// testShares: zfs pengganti dengan dataset di direktori sementara
// (<root>/<pool>/<folder>) dan properti di file samping; setfacl, testparm,
// dan smbcontrol mencatat argumen; ACL isi folder dicatat, tidak dipasang.
func testShares(t *testing.T, testparmExit string) (*Shares, string, string) {
	t.Helper()
	p, _ := newTestPools(t, zpoolFake{names: "tank\n"})
	dir := t.TempDir()
	root := filepath.Join(dir, "mnt")
	props := filepath.Join(dir, "props")
	log := filepath.Join(dir, "calls.log")
	for _, d := range []string{filepath.Join(root, "tank"), props} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		script := "#!/bin/sh\nprintf '%s %s\\n' " + name + " \"$*\" >> '" + log + "'\n" + body
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// Operand terakhir adalah nama dataset (setelah "--").
	zfs := write("zfs", `for a; do ds="$a"; done
key=$(echo "$ds" | tr / _)
prop() { cat "`+props+`/$key.$(echo "$1" | tr : _)" 2>/dev/null || echo -; }
case "$1" in
get)
  if [ "$ds" = tank ]; then printf 'mountpoint\t%s\n' "`+root+`/tank"; exit 0; fi
  [ -d "`+root+`/$ds" ] || { echo "cannot open '$ds': dataset does not exist" >&2; exit 1; }
  for p in $(echo "$6" | tr , ' '); do
    case "$p" in
    type) v=filesystem ;;
    mountpoint) v="`+root+`/$ds" ;;
    mounted) v=yes ;;
    *) v=$(prop "$p") ;;
    esac
    printf '%s\t%s\n' "$p" "$v"
  done ;;
list) [ -d "`+root+`/$ds" ] && echo "$ds" ;;
create)
  mkdir -p "`+root+`/$ds"; shift
  while [ "$1" = -o ]; do echo "${2#*=}" > "`+props+`/$key.$(echo "${2%%=*}" | tr : _)"; shift 2; done ;;
set) echo "${2#*=}" > "`+props+`/$key.$(echo "${2%%=*}" | tr : _)" ;;
destroy) rm -rf "`+root+`/$ds"; rm -f "`+props+`/$key".* ;;
esac
`)
	smbConfDir := filepath.Join(dir, "samba")
	if err := os.MkdirAll(smbConfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	_ = os.WriteFile(passwd, []byte("ani:x:20011:20000::/nonexistent:/usr/sbin/nologin\nbudi:x:20012:20000::/nonexistent:/usr/sbin/nologin\n"), 0o644)
	_ = os.WriteFile(group, []byte("sapo-users:x:20000:\nortu:x:20013:budi\n"), 0o644)
	s := NewShares(p, ShareTools{
		Zfs: zfs, Setfacl: write("setfacl", ""), Testparm: write("testparm", "exit "+testparmExit+"\n"),
		Smbcontrol: write("smbcontrol", ""), SMBConf: filepath.Join(smbConfDir, "smb.conf"),
		Workgroup: "WORKGROUP", ServerString: "Sapo NAS", MinProtocol: "SMB2",
		MountBase: root, PasswdFile: passwd, GroupFile: group,
	})
	// Pekerjaan ACL latar belakang dihentikan sebelum direktori sementara
	// dihapus (cleanup berjalan terbalik, jadi ini lebih dulu).
	t.Cleanup(func() {
		s.jobsMu.Lock()
		var ds []string
		for d := range s.jobs {
			ds = append(ds, d)
		}
		s.jobsMu.Unlock()
		for _, d := range ds {
			s.stopACLJob(d)
		}
	})
	s.chown = func(string, int, int) error { return nil }
	s.isMount = func(string) bool { return true }
	s.aclTree = func(_ context.Context, root string, acl shareACL) error {
		f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o644)
		defer f.Close()
		fmt.Fprintf(f, "acltree %s %v\n", root, acl)
		return nil
	}
	return s, root, log
}

func applyShares(t *testing.T, s *Shares, specs ...proto.ShareSpec) (proto.SharesApplyResult, error) {
	t.Helper()
	raw, _ := json.Marshal(proto.SharesApplyParams{Shares: specs})
	res, err := s.ApplyOp()(context.Background(), raw)
	if err != nil {
		return proto.SharesApplyResult{}, err
	}
	return res.(proto.SharesApplyResult), nil
}

func TestSharesApply(t *testing.T) {
	s, root, log := testShares(t, "0")
	res, err := applyShares(t, s,
		proto.ShareSpec{Name: "keluarga", Pool: "tank", Description: "Foto & dokumen", Read: []string{"user:ani"}, Write: []string{"user:budi", "group:ortu"}},
		proto.ShareSpec{Name: "kosong", Pool: "tank"},
		proto.ShareSpec{Name: "hilang", Pool: "lain", Write: []string{"user:budi"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.SMB != "applied" || len(res.Shares) != 3 || res.Shares[0].Error != "" || res.Shares[0].Path != filepath.Join(root, "tank/keluarga") || res.Shares[2].Error == "" {
		t.Fatalf("hasil = %+v", res)
	}
	info, _ := os.Stat(filepath.Join(root, "tank/keluarga"))
	if info.Mode().Perm() != 0o770 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	calls, _ := os.ReadFile(log)
	acl := "setfacl --physical --set u::rwx,g::---,o::---,m::rwx,u:ani:r-x,u:budi:rwx,g:ortu:rwx,d:u::rwx,d:g::---,d:o::---,d:m::rwx,d:u:ani:r-x,d:u:budi:rwx,d:g:ortu:rwx -- " + filepath.Join(root, "tank/keluarga")
	if !strings.Contains(string(calls), acl) {
		t.Fatalf("setfacl:\n%s", calls)
	}
	conf, _ := os.ReadFile(s.t.SMBConf)
	c := string(conf)
	for _, want := range []string{
		"server min protocol = SMB2", "map to guest = never",
		"[keluarga]\n   comment = Foto & dokumen\n   path = " + filepath.Join(root, "tank/keluarga"),
		"valid users = ani budi +ortu", "write list = budi +ortu",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("smb.conf tanpa %q:\n%s", want, c)
		}
	}
	if !strings.Contains(c, "invalid users = root") {
		t.Fatal("root harus ditolak di Samba")
	}
	// Izin diterapkan ke isi folder di latar belakang, lalu tercatat.
	waitFor(t, func() bool {
		b, _ := os.ReadFile(log)
		return strings.Contains(string(b), "acltree "+filepath.Join(root, "tank/keluarga")+" {[20011] [20012] [] [20013]}") &&
			strings.Contains(string(b), "zfs set sapo:acl=")
	})
	// Diterapkan ulang tanpa perubahan izin: isi folder tidak diproses lagi.
	os.Remove(log)
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "keluarga", Pool: "tank", Description: "Foto & dokumen", Read: []string{"user:ani"}, Write: []string{"user:budi", "group:ortu"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "acltree") {
		t.Fatalf("ACL isi folder tidak perlu diulang:\n%s", b)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "smbcontrol smbd close-share -- kosong") && strings.Contains(c, "[kosong]") {
		t.Fatal("tidak terduga")
	}
	// Folder tanpa izin tidak dibagikan (valid users kosong = semua boleh).
	if strings.Contains(c, "[kosong]") || strings.Contains(c, "[hilang]") {
		t.Fatalf("folder tanpa izin atau tanpa pool tidak boleh dibagikan:\n%s", c)
	}
}

func TestSharesApplyKeepsConfWhenTestparmFails(t *testing.T) {
	s, _, _ := testShares(t, "1")
	if err := os.WriteFile(s.t.SMBConf, []byte("lama\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "data", Pool: "tank", Write: []string{"user:budi"}}); err == nil {
		t.Fatal("testparm gagal harus menjadi error")
	}
	if b, _ := os.ReadFile(s.t.SMBConf); string(b) != "lama\n" {
		t.Fatal("smb.conf lama harus tetap")
	}
}

func TestSharesRejectInjection(t *testing.T) {
	s, _, log := testShares(t, "0")
	var oe *OpError
	for name, sp := range map[string]proto.ShareSpec{
		"baris baru di keterangan": {Name: "data", Pool: "tank", Description: "x\n[global]\npath = /"},
		"kurung siku":              {Name: "data", Pool: "tank", Description: "[x]"},
		"substitusi Samba":         {Name: "data", Pool: "tank", Description: "%m"},
		"nama khusus Samba":        {Name: "homes", Pool: "tank"},
		"nama dengan garis miring": {Name: "a/b", Pool: "tank"},
		"izin aneh":                {Name: "data", Pool: "tank", Write: []string{"user:budi ani"}},
		"izin tanpa jenis":         {Name: "data", Pool: "tank", Write: []string{"budi"}},
		"pool aneh":                {Name: "data", Pool: "../etc"},
	} {
		if _, err := applyShares(t, s, sp); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("tidak boleh ada program yang dijalankan")
	}
}

func TestSharesDelete(t *testing.T) {
	s, root, log := testShares(t, "0")
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "data", Pool: "tank", Write: []string{"user:budi"}}); err != nil {
		t.Fatal(err)
	}
	del := func(name, pool string) error {
		raw, _ := json.Marshal(proto.ShareDeleteParams{Name: name, Pool: pool})
		_, err := s.DeleteOp()(context.Background(), raw)
		return err
	}
	var oe *OpError
	if err := del("lain", "tank"); !errors.As(err, &oe) || oe.Code != proto.ErrNotFound {
		t.Fatalf("folder tidak ada: %v", err)
	}
	if err := del("data", "tank"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tank/data")); err == nil {
		t.Fatal("dataset harus terhapus")
	}
	calls, _ := os.ReadFile(log)
	inOrder(t, string(calls), "smbcontrol smbd close-share -- data", "zfs unmount -- tank/data", "zfs destroy -r -- tank/data")
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("waktu tunggu habis")
}

func TestSharesRefuseForeignDatasets(t *testing.T) {
	s, root, _ := testShares(t, "0")
	// Dataset yang sudah ada tanpa penanda Sapo (misalnya rpool/home).
	if err := os.MkdirAll(filepath.Join(root, "tank/home"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := applyShares(t, s, proto.ShareSpec{Name: "home", Pool: "tank", Write: []string{"user:budi"}})
	if err != nil || !strings.Contains(res.Shares[0].Error, "bukan shared folder Sapo") {
		t.Fatalf("hasil = %+v %v", res, err)
	}
	if b, _ := os.ReadFile(s.t.SMBConf); strings.Contains(string(b), "[home]") {
		t.Fatal("dataset asing tidak boleh dibagikan")
	}
	raw, _ := json.Marshal(proto.ShareDeleteParams{Name: "home", Pool: "tank"})
	var oe *OpError
	if _, err := s.DeleteOp()(context.Background(), raw); !errors.As(err, &oe) || oe.Code != proto.ErrNotFound {
		t.Fatalf("dataset asing tidak boleh dihapus: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tank/home")); err != nil {
		t.Fatal("dataset asing hilang")
	}
	// Pool yang tidak terpasang di mount_base (misalnya root-on-ZFS).
	s.t.MountBase = "/srv/lain"
	res, _ = applyShares(t, s, proto.ShareSpec{Name: "etc", Pool: "tank", Write: []string{"user:budi"}})
	if !strings.Contains(res.Shares[0].Error, "tidak terpasang") {
		t.Fatalf("hasil = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "tank/etc")); err == nil {
		t.Fatal("dataset tidak boleh dibuat di pool asing")
	}
}
