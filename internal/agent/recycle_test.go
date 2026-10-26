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

func TestSharesRecycle(t *testing.T) {
	s, root, _ := testShares(t, "0")
	s.t.RecycleExclude = []string{"~$*", "*.tmp", "jahat\n[global]", "a,b"}
	spec := proto.ShareSpec{Name: "keluarga", Pool: "tank", Write: []string{"user:budi"}, RecycleDays: 30}
	res, err := applyShares(t, s, spec)
	if err != nil || res.Shares[0].RecycleError != "" {
		t.Fatalf("%v %+v", err, res)
	}
	if fi, err := os.Lstat(filepath.Join(root, "tank/keluarga", recycleDir)); err != nil || !fi.IsDir() {
		t.Fatalf("#recycle harus dibuat: %v", err)
	}
	props, _ := s.zfsProps(context.Background(), "tank/keluarga", recyclePropName)
	if props[recyclePropName] != "30" {
		t.Fatalf("sapo:recycle = %q", props[recyclePropName])
	}
	conf, _ := os.ReadFile(s.t.SMBConf)
	c := string(conf)
	for _, want := range []string{"vfs objects = shadow_copy2 recycle", "recycle:repository = #recycle", "recycle:keeptree = yes", "recycle:exclude = ~$*,*.tmp\n"} {
		if !strings.Contains(c, want) {
			t.Errorf("smb.conf tanpa %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "jahat") || strings.Count(c, "[global]") != 1 {
		t.Fatalf("pola exclude yang tidak aman masuk smb.conf:\n%s", c)
	}
	// Dimatikan: sapo:recycle off, isi #recycle tidak disentuh, SMB tanpa recycle.
	_ = os.WriteFile(filepath.Join(root, "tank/keluarga", recycleDir, "lama.txt"), []byte("x"), 0o644)
	spec.RecycleDays = 0
	if _, err := applyShares(t, s, spec); err != nil {
		t.Fatal(err)
	}
	props, _ = s.zfsProps(context.Background(), "tank/keluarga", recyclePropName)
	conf, _ = os.ReadFile(s.t.SMBConf)
	if props[recyclePropName] != "off" || strings.Contains(string(conf), "recycle:") {
		t.Fatalf("mati: %q\n%s", props[recyclePropName], conf)
	}
	if _, err := os.Stat(filepath.Join(root, "tank/keluarga", recycleDir, "lama.txt")); err != nil {
		t.Fatal("isi tempat sampah tidak boleh dibuang saat dimatikan")
	}
	// #recycle bukan folder: tempat sampah tidak dinyalakan (SMB akan
	// menghapus langsung bila folder itu tidak bisa dibuat).
	_ = os.RemoveAll(filepath.Join(root, "tank/keluarga", recycleDir))
	_ = os.WriteFile(filepath.Join(root, "tank/keluarga", recycleDir), []byte("x"), 0o644)
	s.mkRecycle = func(string) error { return errors.New("#recycle bukan folder") }
	spec.RecycleDays = 7
	res, _ = applyShares(t, s, spec)
	conf, _ = os.ReadFile(s.t.SMBConf)
	if res.Shares[0].RecycleError == "" || strings.Contains(string(conf), "recycle:") {
		t.Fatalf("gagal disiapkan: %+v\n%s", res, conf)
	}
	// Lama simpan di luar batas ditolak.
	if err := s.setRecycle(context.Background(), "tank/keluarga", filepath.Join(root, "tank/keluarga"), proto.MaxRecycleDays+1); err == nil {
		t.Fatal("lama simpan terlalu besar harus ditolak")
	}
}

func TestRecycleTickAndEmpty(t *testing.T) {
	s, root, _ := testShares(t, "0")
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "keluarga", Pool: "tank", Write: []string{"user:budi"}, RecycleDays: 30},
		proto.ShareSpec{Name: "kantor", Pool: "tank", Write: []string{"user:budi"}}); err != nil {
		t.Fatal(err)
	}
	r := NewRecycle(s, RecycleTools{})
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	type call struct {
		root   string
		cutoff time.Time
	}
	var calls []call
	r.clean = func(root string, cutoff, _ time.Time) (recycleStats, error) {
		calls = append(calls, call{root, cutoff})
		return recycleStats{Bytes: 10, Files: 1, Removed: 2, Done: true}, nil
	}
	out := "tank\t-\t-\tyes\t" + filepath.Join(root, "tank") + "\n" +
		"tank/keluarga\tkeluarga\t30\tyes\t" + filepath.Join(root, "tank/keluarga") + "\n" +
		"tank/kantor\tkantor\toff\tyes\t" + filepath.Join(root, "tank/kantor") + "\n" +
		"tank/palsu\tlain\t30\tyes\t" + filepath.Join(root, "tank/palsu") + "\n" +
		"tank/geser\tgeser\t30\tyes\t/etc\n" +
		"tank/lepas\tlepas\t30\tno\t" + filepath.Join(root, "tank/lepas") + "\n" +
		"tank/a/b\tb\t30\tyes\t" + filepath.Join(root, "tank/a/b") + "\n"
	r.listFn = func(context.Context) ([]recycleShare, error) { return parseRecycleList([]byte(out), root), nil }
	r.Tick(context.Background())
	if len(calls) != 2 || calls[0].root != filepath.Join(root, "tank/keluarga") || !calls[0].cutoff.Equal(now.Add(-30*24*time.Hour)) || !calls[1].cutoff.IsZero() {
		t.Fatalf("pembersihan = %+v", calls)
	}
	st, _ := r.StatusOp()(context.Background(), nil)
	if u := st.([]proto.RecycleUsage); len(u) != 2 || u[0].Share != "kantor" || u[1].Days != 30 || u[1].Bytes != 10 {
		t.Fatalf("status = %+v", u)
	}
	empty := func(p proto.RecycleTarget) (any, error) {
		raw, _ := json.Marshal(p)
		return r.EmptyOp()(context.Background(), raw)
	}
	var oe *OpError
	for _, p := range []proto.RecycleTarget{{Pool: "tank", Share: "palsu"}, {Pool: "tank", Share: "../x"}} {
		if _, err := empty(p); !errors.As(err, &oe) {
			t.Errorf("%+v harus ditolak: %v", p, err)
		}
	}
	calls = nil
	res, err := empty(proto.RecycleTarget{Pool: "tank", Share: "keluarga"})
	if err != nil || len(calls) != 1 || !calls[0].cutoff.After(now) || res.(proto.RecycleEmptyResult).Removed != 2 {
		t.Fatalf("kosongkan: %v %+v %+v", err, res, calls)
	}
}
