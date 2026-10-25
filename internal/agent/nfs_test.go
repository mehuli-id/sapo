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

// testNFS: testShares ditambah exportfs dan systemctl pengganti yang
// mencatat argumen ke log yang sama.
func testNFS(t *testing.T, exportfsExit string) (*Shares, string, string) {
	t.Helper()
	s, root, log := testShares(t, "0")
	dir := filepath.Dir(log)
	for name, body := range map[string]string{"exportfs": "exit " + exportfsExit + "\n", "systemctl": ""} {
		script := "#!/bin/sh\nprintf '%s %s\\n' " + name + " \"$*\" >> '" + log + "'\n" + body
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.t.Exportfs, s.t.Systemctl = filepath.Join(dir, "exportfs"), filepath.Join(dir, "systemctl")
	s.t.ExportsFile, s.t.NFSService = filepath.Join(dir, "exports.d", "sapo.exports"), "nfs-server.service"
	s.t.IDMin, s.t.IDMax = 20000, 29999
	return s, root, log
}

func TestSharesNFSExport(t *testing.T) {
	s, root, log := testNFS(t, "0")
	res, err := applyShares(t, s,
		// ani mendapat akses lewat grup: entri ACL ani ditambahkan untuk NFS.
		proto.ShareSpec{Name: "keluarga", Pool: "tank", Write: []string{"group:ortu"},
			NFS: &proto.NFSExport{Clients: []string{"192.168.1.0/24", "10.0.2.2/32"}, User: "ani"}},
		proto.ShareSpec{Name: "foto", Pool: "tank", Read: []string{"user:budi"},
			NFS: &proto.NFSExport{Clients: []string{"fd00::/64"}, User: "budi", ReadOnly: true}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.NFS != "applied" || res.Shares[0].NFSError != "" {
		t.Fatalf("hasil = %+v", res)
	}
	exports, _ := os.ReadFile(s.t.ExportsFile)
	want := filepath.Join(root, "tank/keluarga") +
		" 192.168.1.0/24(sec=sys,rw," + nfsOptions + ",anonuid=20011,anongid=20000)" +
		" 10.0.2.2/32(sec=sys,rw," + nfsOptions + ",anonuid=20011,anongid=20000)\n"
	if !strings.Contains(string(exports), want) || !strings.Contains(string(exports), " fd00::/64(sec=sys,ro,") {
		t.Fatalf("file ekspor:\n%s", exports)
	}
	calls, _ := os.ReadFile(log)
	c := string(calls)
	for _, w := range []string{
		"g:ortu:rwx,u:ani:rwx", // akun NFS punya entri ACL sendiri
		"m::rwx,u:budi:r-x,d:", // tanpa entri ganda
	} {
		if !strings.Contains(c, w) {
			t.Errorf("panggilan tanpa %q:\n%s", w, c)
		}
	}
	inOrder(t, c, "systemctl enable --now -- nfs-server.service", "exportfs -ra --")
	// Samba tetap memakai izin asli (ani tidak ditambahkan ke valid users).
	if conf, _ := os.ReadFile(s.t.SMBConf); strings.Contains(string(conf), "ani") {
		t.Fatalf("smb.conf:\n%s", conf)
	}

	// Ekspor terakhir dicabut: file kosong, server NFS dihentikan.
	os.Remove(log)
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "keluarga", Pool: "tank", Write: []string{"group:ortu"}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(s.t.ExportsFile); strings.Contains(string(b), root) {
		t.Fatalf("ekspor harus kosong:\n%s", b)
	}
	b, _ := os.ReadFile(log)
	inOrder(t, string(b), "exportfs -ra --", "systemctl disable --now -- nfs-server.service")
}

func TestSharesNFSRejectsBadSpecs(t *testing.T) {
	s, _, _ := testNFS(t, "0")
	var oe *OpError
	for name, clients := range map[string][]string{
		"semua orang":       {"0.0.0.0/0"},
		"terlalu luas":      {"10.0.0.0/7"},
		"bukan kanonis":     {"192.168.1.5/24"},
		"tanpa prefiks":     {"192.168.1.5"},
		"nama host":         {"nas.example"},
		"opsi disisipkan":   {"10.0.0.0/8(rw,no_root_squash)"},
		"ganda":             {"10.0.0.0/8", "10.0.0.0/8"},
		"IPv6 terlalu luas": {"::/0"},
		"jaringan publik":   {"8.8.8.0/24"},
		"IPv6 publik":       {"2001:db8::/64"},
		"kosong":            {},
	} {
		sp := proto.ShareSpec{Name: "data", Pool: "tank", Write: []string{"user:budi"}, NFS: &proto.NFSExport{Clients: clients, User: "budi"}}
		if _, err := applyShares(t, s, sp); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Akun di luar rentang Sapo (misalnya root) tidak pernah dipakai.
	_ = os.WriteFile(s.t.PasswdFile, []byte("root:x:0:0::/root:/bin/sh\nbudi:x:20012:20000::/nonexistent:/usr/sbin/nologin\n"), 0o644)
	res, err := applyShares(t, s, proto.ShareSpec{Name: "data", Pool: "tank", Write: []string{"user:budi"}, NFS: &proto.NFSExport{Clients: []string{"10.0.0.0/8"}, User: "root"}})
	if err != nil || res.Shares[0].Error != "" || !strings.Contains(res.Shares[0].NFSError, "bukan akun Sapo") {
		t.Fatalf("hasil = %+v %v", res, err)
	}
	if b, _ := os.ReadFile(s.t.ExportsFile); strings.Contains(string(b), "anonuid=0") || strings.Contains(string(b), "/data") {
		t.Fatalf("file ekspor:\n%s", b)
	}
}

func TestSharesNFSFailsClosedOnExportfsFailure(t *testing.T) {
	s, _, log := testNFS(t, "1")
	if err := os.MkdirAll(filepath.Dir(s.t.ExportsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.t.ExportsFile, []byte("lama\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sp := proto.ShareSpec{Name: "data", Pool: "tank", Write: []string{"user:budi"}, NFS: &proto.NFSExport{Clients: []string{"10.0.0.0/8"}, User: "budi"}}
	if _, err := applyShares(t, s, sp); err == nil {
		t.Fatal("exportfs gagal harus menjadi error")
	}
	// File lama tidak dipulihkan (bisa memberi akses yang baru dicabut):
	// semua ekspor Sapo dicabut dan server NFS dimatikan.
	if b, _ := os.ReadFile(s.t.ExportsFile); string(b) != string(renderExports(nil)) {
		t.Fatalf("file ekspor harus kosong: %q", b)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "systemctl disable --now -- nfs-server.service") {
		t.Fatalf("server NFS harus dimatikan:\n%s", b)
	}
}

func TestSharesDeleteUnexportsFirst(t *testing.T) {
	s, root, log := testNFS(t, "0")
	nfs := &proto.NFSExport{Clients: []string{"10.0.0.0/8"}, User: "budi"}
	if _, err := applyShares(t, s,
		proto.ShareSpec{Name: "data", Pool: "tank", Write: []string{"user:budi"}, NFS: nfs},
		proto.ShareSpec{Name: "foto", Pool: "tank", Write: []string{"user:budi"}, NFS: nfs},
	); err != nil {
		t.Fatal(err)
	}
	os.Remove(log)
	raw, _ := json.Marshal(proto.ShareDeleteParams{Name: "data", Pool: "tank"})
	if _, err := s.DeleteOp()(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s.t.ExportsFile)
	if strings.Contains(string(b), filepath.Join(root, "tank/data")+" ") || !strings.Contains(string(b), filepath.Join(root, "tank/foto")+" ") {
		t.Fatalf("file ekspor:\n%s", b)
	}
	calls, _ := os.ReadFile(log)
	inOrder(t, string(calls), "exportfs -ra --", "zfs unmount -- tank/data")
	if strings.Contains(string(calls), "systemctl disable") {
		t.Fatalf("server NFS tidak boleh dimatikan selama masih ada ekspor:\n%s", calls)
	}
}

func TestPoolDestroyUnexportsShares(t *testing.T) {
	s, root, _ := testNFS(t, "0")
	nfs := &proto.NFSExport{Clients: []string{"10.0.0.0/8"}, User: "budi"}
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "data", Pool: "tank", Write: []string{"user:budi"}, NFS: nfs}); err != nil {
		t.Fatal(err)
	}
	if err := s.p.beforeDestroy[0](context.Background(), "tank"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(s.t.ExportsFile); strings.Contains(string(b), filepath.Join(root, "tank")) {
		t.Fatalf("ekspor folder di pool yang dihapus harus dicabut:\n%s", b)
	}
}

func TestSharesApplyExportsEvenWhenSMBFails(t *testing.T) {
	s, root, _ := testNFS(t, "0")
	nfs := &proto.NFSExport{Clients: []string{"10.0.0.0/8"}, User: "budi"}
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "data", Pool: "tank", Write: []string{"user:budi"}, NFS: nfs}); err != nil {
		t.Fatal(err)
	}
	// testparm menolak: ekspor NFS tetap dicabut sesuai daftar baru.
	if err := os.WriteFile(s.t.Testparm, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "data", Pool: "tank", Write: []string{"user:budi"}}); err == nil {
		t.Fatal("galat Samba harus dilaporkan")
	}
	if b, _ := os.ReadFile(s.t.ExportsFile); strings.Contains(string(b), filepath.Join(root, "tank/data")) {
		t.Fatalf("ekspor harus dicabut walau Samba gagal:\n%s", b)
	}
}

// inOrder: setiap baris want muncul di calls berurutan. Pekerjaan ACL latar
// belakang boleh menyisipkan baris di antaranya.
func inOrder(t *testing.T, calls string, want ...string) {
	t.Helper()
	rest := calls
	for _, w := range want {
		i := strings.Index(rest, w+"\n")
		if i < 0 {
			t.Fatalf("panggilan tanpa %q (berurutan):\n%s", w, calls)
		}
		rest = rest[i+len(w):]
	}
}

func TestSharesDenyAppNetwork(t *testing.T) {
	s, _, _ := testNFS(t, "0")
	s.t.DenyNetworks = []string{"10.88.0.0/16"}
	var oe *OpError
	nfs := &proto.NFSExport{Clients: []string{"10.0.0.0/8"}, User: "budi"}
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "data", Pool: "tank", Write: []string{"user:budi"}, NFS: nfs}); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
		t.Fatalf("klien NFS yang mencakup jaringan aplikasi: %v", err)
	}
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "data", Pool: "tank", Write: []string{"user:budi"}}); err != nil {
		t.Fatal(err)
	}
	if conf, _ := os.ReadFile(s.t.SMBConf); !strings.Contains(string(conf), "hosts deny = 10.88.0.0/16") {
		t.Fatalf("smb.conf:\n%s", conf)
	}
}
