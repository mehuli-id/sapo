package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestCleanFilePath(t *testing.T) {
	for _, ok := range []string{"", "a", "foto/2026/a b.jpg", ".tersembunyi", "dokumen/ñ.txt"} {
		if _, err := CleanFilePath(ok); err != nil {
			t.Errorf("%q harus diterima: %v", ok, err)
		}
	}
	for _, bad := range []string{"/a", "a/", "a//b", ".", "..", "a/../b", ".zfs", "a/.zfs/snapshot", "a\x00b", "a\nb", ".sapo-upload-0123456789abcdef", string(make([]byte, 256))} {
		if _, err := CleanFilePath(bad); err == nil {
			t.Errorf("%q harus ditolak", bad)
		}
	}
}

// testFiles: folder Sapo "keluarga" di pool tank (zfs tiruan dari
// testShares) dan akun budi (anggota ortu dan grup sistem sudo).
func testFiles(t *testing.T) (*Files, *[]fsRequest, *fsCred) {
	t.Helper()
	s, _, _ := testShares(t, "0")
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "keluarga", Pool: "tank", Write: []string{"user:budi"}}); err != nil {
		t.Fatal(err)
	}
	group := filepath.Join(t.TempDir(), "group")
	_ = os.WriteFile(group, []byte("sudo:x:27:budi\nsapo-users:x:20000:\nortu:x:20013:budi,ani\nanak:x:20014:ani\n"), 0o644)
	f := NewFiles(FilesTools{MountBase: s.t.MountBase, GroupFile: group, MaxEntries: 100, Timeout: 1e9, DeleteTimeout: 1e9, IDMin: 20000, IDMax: 29999}, s,
		func(name string) (int, int, bool) { return 20012, 20000, name == "budi" })
	reqs := &[]fsRequest{}
	cred := &fsCred{}
	f.run = func(_ context.Context, c fsCred, req fsRequest, wantFile bool) (fsReply, *os.File, error) {
		*reqs = append(*reqs, req)
		*cred = c
		if req.Path == "terlarang" {
			return fsReply{Code: errFilePermission, Message: "permission denied"}, nil, nil
		}
		if req.Op == "list" {
			return fsReply{Result: json.RawMessage(`{"entries":[]}`)}, nil, nil
		}
		return fsReply{}, nil, nil
	}
	return f, reqs, cred
}

func TestFilesOps(t *testing.T) {
	f, reqs, cred := testFiles(t)
	call := func(h Handler, p any) error {
		raw, _ := json.Marshal(p)
		_, err := h(context.Background(), raw)
		return err
	}
	target := proto.FilesTarget{User: "budi", Pool: "tank", Share: "keluarga", Path: "foto"}
	if err := call(f.ListOp(), target); err != nil {
		t.Fatal(err)
	}
	// Sebagai akun budi dengan grup Sapo saja (grup sistem sudo tidak).
	if cred.UID != 20012 || cred.GID != 20000 || len(cred.Groups) != 2 || cred.Groups[1] != 20013 {
		t.Fatalf("identitas = %+v", cred)
	}
	if r := (*reqs)[0]; r.Op != "list" || r.Path != "foto" || r.Root != filepath.Join(f.t.MountBase, "tank/keluarga") {
		t.Fatalf("permintaan = %+v", r)
	}
	var oe *OpError
	for name, p := range map[string]proto.FilesTarget{
		"akun bukan Sapo":   {User: "root", Pool: "tank", Share: "keluarga"},
		"folder bukan Sapo": {User: "budi", Pool: "tank", Share: "lain"},
		"path keluar":       {User: "budi", Pool: "tank", Share: "keluarga", Path: "../etc"},
		"pool aneh":         {User: "budi", Pool: "../x", Share: "keluarga"},
	} {
		if err := call(f.ListOp(), p); !errors.As(err, &oe) {
			t.Errorf("%s harus ditolak: %v", name, err)
		}
	}
	if err := call(f.DeleteOp(), proto.FilesTarget{User: "budi", Pool: "tank", Share: "keluarga"}); !errors.As(err, &oe) {
		t.Fatalf("akar folder tidak boleh dihapus: %v", err)
	}
	if err := call(f.MkdirOp(), proto.FilesTarget{User: "budi", Pool: "tank", Share: "keluarga", Path: "terlarang"}); !errors.As(err, &oe) || oe.Code != errFilePermission {
		t.Fatalf("galat izin dari kernel diteruskan: %v", err)
	}
	if err := call(f.CommitOp(), proto.FilesCommitParams{FilesTarget: target, Temp: "../../etc/passwd"}); !errors.As(err, &oe) {
		t.Fatalf("nama sementara aneh: %v", err)
	}
	if err := call(f.RenameOp(), proto.FilesRenameParams{FilesTarget: target, To: "../x"}); !errors.As(err, &oe) {
		t.Fatalf("rename keluar: %v", err)
	}
}

func TestFilesLimitsConcurrency(t *testing.T) {
	f, _, _ := testFiles(t)
	for i := 0; i < maxFileOps; i++ {
		f.slots <- struct{}{}
	}
	raw, _ := json.Marshal(proto.FilesTarget{User: "budi", Pool: "tank", Share: "keluarga"})
	var oe *OpError
	if _, err := f.ListOp()(context.Background(), raw); !errors.As(err, &oe) || oe.Code != proto.ErrBusy {
		t.Fatalf("slot penuh: %v", err)
	}
	for i := 0; i < maxFileOps; i++ {
		<-f.slots
	}
	f.deleting["budi"] = true
	raw, _ = json.Marshal(proto.FilesTarget{User: "budi", Pool: "tank", Share: "keluarga", Path: "a"})
	if _, err := f.DeleteOp()(context.Background(), raw); !errors.As(err, &oe) || oe.Code != proto.ErrBusy {
		t.Fatalf("hapus kedua: %v", err)
	}
}
