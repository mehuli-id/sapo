package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

const lsblkUSB = `{"blockdevices":[
 {"name":"vda","path":"/dev/vda","type":"disk","size":1000,"tran":"virtio","fstype":"ext4","uuid":"SYS-1","mountpoints":["/"]},
 {"name":"sdb","path":"/dev/sdb","type":"disk","size":64000,"model":"Flash","serial":"F1","tran":"usb","fstype":"exfat","label":"FOTO","uuid":"AAAA-1111","mountpoints":[null]},
 {"name":"sdc","path":"/dev/sdc","type":"disk","size":2000,"model":"Dock","serial":"BRIDGE","tran":"usb","pttype":"gpt","children":[
   {"name":"sdc1","path":"/dev/sdc1","type":"part","size":1000,"fstype":"ntfs","label":"DATA","uuid":"0123456789ABCDEF","mountpoints":[null]},
   {"name":"sdc2","path":"/dev/sdc2","type":"part","size":1000,"fstype":"ext4","uuid":"e1e1e1e1-0000-1111-2222-333333333333","mountpoints":[null]}]},
 {"name":"sdd","path":"/dev/sdd","type":"disk","size":2000,"serial":"BRIDGE","tran":"usb","fstype":"zfs_member","uuid":"123","mountpoints":[null]}
]}`

func testExternal(t *testing.T) (*External, string, string) {
	t.Helper()
	s, _, _ := testShares(t, "0")
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	lsblkOut := filepath.Join(dir, "lsblk.json")
	_ = os.WriteFile(lsblkOut, []byte(lsblkUSB), 0o644)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s %s\\n' "+name+" \"$*\" >> '"+log+"'\n"+body), 0o755)
		return p
	}
	base := filepath.Join(dir, "ext")
	e := NewExternal(ExternalTools{
		Mount: write("mount", ""), Umount: write("umount", ""), Lsblk: write("lsblk", "cat '"+lsblkOut+"'\n"),
		MountBase: base, StateFile: filepath.Join(dir, "external.json"), GroupFile: s.t.GroupFile,
		IDMin: 20000, IDMax: 29999, Timeout: 5 * time.Second,
	}, NewStorage(StorageTools{Timeout: 5 * time.Second}), s)
	e.rootOK = func(string, int) error { return nil }
	// Titik pasang "terpasang" setelah mount tiruan dipanggil.
	e.isMount = func(p string) bool {
		b, _ := os.ReadFile(log)
		mounted := false
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasSuffix(line, " "+p) {
				mounted = strings.HasPrefix(line, "mount ")
			}
		}
		return mounted
	}
	return e, log, lsblkOut
}

func extCall(t *testing.T, h Handler, p any) (any, error) {
	t.Helper()
	raw, _ := json.Marshal(p)
	return h(context.Background(), raw)
}

func TestExternalListAndMount(t *testing.T) {
	e, log, lsblkOut := testExternal(t)
	res, err := extCall(t, e.ListOp(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]proto.ExternalVolume{}
	for _, v := range res.([]proto.ExternalVolume) {
		got[v.Device] = v
	}
	if len(got) != 4 || !got["sdb"].Supported || !got["sdc1"].Supported || got["sdc2"].Reason != "unsupported_fs" || got["sdd"].Reason != "member" {
		t.Fatalf("volume = %+v", got)
	}
	var oe *OpError
	for name, p := range map[string]proto.ExternalMountParams{
		"grup sistem":      {UUID: "AAAA-1111", Name: "foto", Group: "sudo"},
		"tidak ada":        {UUID: "BBBB-2222", Name: "foto", Group: "ortu"},
		"format linux":     {UUID: "e1e1e1e1-0000-1111-2222-333333333333", Name: "foto", Group: "ortu"},
		"anggota pool":     {UUID: "123", Name: "foto", Group: "ortu"},
		"nama dicadangkan": {UUID: "AAAA-1111", Name: "homes", Group: "ortu"},
		"nama aneh":        {UUID: "AAAA-1111", Name: "../etc", Group: "ortu"},
	} {
		if _, err := extCall(t, e.MountOp(), p); !errors.As(err, &oe) {
			t.Errorf("%s harus ditolak: %v", name, err)
		}
	}
	if _, err := extCall(t, e.MountOp(), proto.ExternalMountParams{UUID: "AAAA-1111", Name: "foto", Group: "ortu", ReadOnly: true, SMB: true}); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	mp := filepath.Join(e.t.MountBase, "foto")
	if !strings.Contains(string(calls), "mount -t exfat -o nosuid,nodev,noexec,uid=0,gid=20013,ro,fmask=0137,dmask=0027 -- /dev/sdb "+mp) {
		t.Fatalf("mount:\n%s", calls)
	}
	if _, err := extCall(t, e.MountOp(), proto.ExternalMountParams{UUID: "AAAA-1111", Name: "foto2", Group: "ortu"}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("drive dipasang dua kali: %v", err)
	}
	// SMB: valid users dari grup, baca saja.
	conf := string(e.shares.renderSMBConf(nil, "", e.shares.externalSMB(nil, false)))
	if !strings.Contains(conf, "[foto]\n") || !strings.Contains(conf, "valid users = +ortu\n") || !strings.Contains(conf, "read only = yes\n") {
		t.Fatalf("smb.conf:\n%s", conf)
	}
	if e.shares.externalSMB(nil, true) != nil {
		t.Fatal("SMB mati: folder eksternal tidak dibagikan")
	}
	// File Station: hanya folder yang terpasang.
	if root, ok := e.root("foto"); !ok || root != mp {
		t.Fatalf("akar = %q %v", root, ok)
	}
	// Drive dicabut tanpa dilepas: titik pasang dilepas (lazy), status terputus.
	_ = os.WriteFile(lsblkOut, []byte(strings.Replace(lsblkUSB, `"AAAA-1111"`, `"XXXX-0000"`, 1)), 0o644)
	// Sekali tidak terlihat (USB direset sesaat): belum dilepas.
	_, _ = extCall(t, e.ListOp(), nil)
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "umount -l") {
		t.Fatal("drive yang sesaat tidak terlihat tidak boleh langsung dilepas")
	}
	_, _ = extCall(t, e.ListOp(), nil)
	res, _ = extCall(t, e.ListOp(), nil)
	for _, v := range res.([]proto.ExternalVolume) {
		if v.Name == "foto" && v.State != "disconnected" {
			t.Fatalf("status = %+v", v)
		}
	}
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "umount -l -- "+mp) {
		t.Fatalf("lazy umount:\n%s", calls)
	}
	// Dicolok lagi: dipasang lagi otomatis.
	_ = os.WriteFile(lsblkOut, []byte(lsblkUSB), 0o644)
	_ = os.Truncate(log, 0)
	res, _ = extCall(t, e.ListOp(), nil)
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "mount -t exfat") {
		t.Fatalf("dipasang lagi: %+v\n%s", res, calls)
	}
	// Dilepas: umount biasa (tidak lazy), konfigurasi dilupakan.
	before, _ := os.ReadFile(log)
	if _, err := extCall(t, e.UnmountOp(), proto.ExternalParams{Name: "foto"}); err != nil {
		t.Fatal(err)
	}
	calls, _ = os.ReadFile(log)
	calls = calls[len(before):]
	if !strings.Contains(string(calls), "umount -- "+mp) || strings.Contains(string(calls), "umount -l") {
		t.Fatalf("lepas:\n%s", calls)
	}
	if st, _ := e.load(); len(st.Volumes) != 0 {
		t.Fatalf("konfigurasi masih ada: %+v", st)
	}
}

func TestExternalReadWriteNTFS(t *testing.T) {
	e, log, _ := testExternal(t)
	if _, err := extCall(t, e.MountOp(), proto.ExternalMountParams{UUID: "0123456789ABCDEF", Name: "dock", Group: "sapo-users"}); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "mount -t ntfs3 -o nosuid,nodev,noexec,uid=0,gid=20000,fmask=0117,dmask=0007 -- /dev/sdc1 ") {
		t.Fatalf("mount NTFS:\n%s", calls)
	}
}

// Drive hasil kloning: UUID kembar tidak dipasang, dan drive lain dengan UUID
// sama (ukuran/nomor seri berbeda) tidak dipasang otomatis.
func TestExternalCloneGuards(t *testing.T) {
	e, log, lsblkOut := testExternal(t)
	if _, err := extCall(t, e.MountOp(), proto.ExternalMountParams{UUID: "AAAA-1111", Name: "foto", Group: "ortu"}); err != nil {
		t.Fatal(err)
	}
	if _, err := extCall(t, e.UnmountOp(), proto.ExternalParams{Name: "foto"}); err != nil {
		t.Fatal(err)
	}
	// Dua volume ber-UUID sama.
	dup := strings.Replace(lsblkUSB, `"0123456789ABCDEF"`, `"AAAA-1111"`, 1)
	_ = os.WriteFile(lsblkOut, []byte(dup), 0o644)
	var oe *OpError
	if _, err := extCall(t, e.MountOp(), proto.ExternalMountParams{UUID: "AAAA-1111", Name: "foto", Group: "ortu"}); !errors.As(err, &oe) || !strings.Contains(oe.Message, "duplicate_uuid") {
		t.Fatalf("UUID kembar: %v", err)
	}
	// Dipasang, lalu drive diganti drive lain ber-UUID sama (ukuran lain).
	_ = os.WriteFile(lsblkOut, []byte(lsblkUSB), 0o644)
	if _, err := extCall(t, e.MountOp(), proto.ExternalMountParams{UUID: "AAAA-1111", Name: "foto", Group: "ortu"}); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(e.t.MountBase, "foto")
	before, _ := os.ReadFile(log)
	_ = os.WriteFile(lsblkOut, []byte(strings.Replace(lsblkUSB, `"size":64000`, `"size":64001`, 1)), 0o644)
	res, _ := extCall(t, e.ListOp(), nil)
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls[len(before):]), "umount -- "+mp) {
		t.Fatalf("drive lain harus dilepas:\n%s", calls[len(before):])
	}
	for _, v := range res.([]proto.ExternalVolume) {
		if v.Name == "foto" && v.State != "error" {
			t.Fatalf("status drive lain = %+v", v)
		}
	}
	before, _ = os.ReadFile(log)
	_, _ = extCall(t, e.ListOp(), nil)
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls[len(before):]), "mount -t") {
		t.Fatal("drive lain tidak boleh dipasang otomatis")
	}
}

// Grup yang dihapus lalu dibuat ulang (GID lain) tidak mewarisi akses.
func TestExternalGroupChanged(t *testing.T) {
	e, _, _ := testExternal(t)
	err := e.mountVol(context.Background(), "foto", externalConf{UUID: "AAAA-1111", FSType: "exfat", Group: "ortu", GID: 20999}, "/dev/sdb")
	var oe *OpError
	if !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("grup berubah: %v", err)
	}
}
