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

// testShares: zfs pengganti dengan dataset di direktori sementara
// (<root>/<pool>/<folder>), setfacl/testparm/smbcontrol yang mencatat argumen.
func testShares(t *testing.T, testparmExit string) (*Shares, string, string) {
	t.Helper()
	p, _ := newTestPools(t, zpoolFake{names: "tank\n"})
	dir := t.TempDir()
	root := filepath.Join(dir, "mnt")
	log := filepath.Join(dir, "calls.log")
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
case "$1" in
list) [ -d "`+root+`/$ds" ] || exit 1; [ "$4" = "name,type" ] && printf '%s\tfilesystem\n' "$ds" || echo "$ds" ;;
create) mkdir -p "`+root+`/$ds" ;;
get) echo "`+root+`/$ds" ;;
destroy) rm -rf "`+root+`/$ds" ;;
esac
`)
	smbConfDir := filepath.Join(dir, "samba")
	if err := os.MkdirAll(smbConfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewShares(p, ShareTools{
		Zfs: zfs, Setfacl: write("setfacl", ""), Testparm: write("testparm", "exit "+testparmExit+"\n"),
		Smbcontrol: write("smbcontrol", ""), SMBConf: filepath.Join(smbConfDir, "smb.conf"),
		Workgroup: "WORKGROUP", ServerString: "Sapo NAS", MinProtocol: "SMB2",
	})
	s.chown = func(string, int, int) error { return nil }
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
	if !strings.Contains(string(calls), "smbcontrol smbd close-share -- data") || !strings.Contains(string(calls), "zfs destroy -r -- tank/data") {
		t.Fatalf("panggilan:\n%s", calls)
	}
}
