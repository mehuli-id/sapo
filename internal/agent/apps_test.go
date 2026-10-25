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

const testDigest = "sha256:febb5c423e5fb969ddcad7ad2b344de37bb47cd8c16c14df23e9150f5ddd3171"

// testApps: pool tank dengan zfs tiruan (testShares), katalog berisi
// homer, useradd/groupadd/systemctl tiruan yang mencatat argumen.
func testApps(t *testing.T) (*Apps, string, string) {
	t.Helper()
	s, root, log := testShares(t, "0")
	dir := filepath.Dir(log)
	cat := filepath.Join(dir, "catalog")
	_ = os.MkdirAll(cat, 0o755)
	_ = os.WriteFile(filepath.Join(cat, "homer.yaml"), []byte(`id: homer
name: { id: Homer, en: Homer }
description: { id: Halaman awal, en: Start page }
image: docker.io/b4bz/homer:v25.09.1@`+testDigest+`
web_port: 8080
default_port: 8090
data_path: /www/assets
env: { TZ: Asia/Jakarta }
memory_mb: 128
`), 0o644)
	// Entri tidak valid (image tanpa digest, opsi berbahaya diabaikan) dilewati.
	_ = os.WriteFile(filepath.Join(cat, "jahat.yaml"), []byte("id: jahat\nimage: docker.io/x/y:latest\nweb_port: 80\ndefault_port: 8000\ndata_path: /data\nmemory_mb: 64\n"), 0o644)
	for _, name := range []string{"useradd", "userdel", "groupadd", "groupdel", "systemctl"} {
		script := "#!/bin/sh\nprintf '%s %s\\n' " + name + " \"$*\" >> '" + log + "'\n"
		if name == "systemctl" {
			script += "case \"$1\" in is-active) echo active ;; esac\n"
		}
		_ = os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755)
	}
	a := NewApps(AppsTools{
		CatalogDir: cat, Podman: "/usr/bin/podman", QuadletDir: filepath.Join(dir, "quadlet"), StateFile: filepath.Join(dir, "apps.json"),
		Systemctl: filepath.Join(dir, "systemctl"), Useradd: filepath.Join(dir, "useradd"), Userdel: filepath.Join(dir, "userdel"),
		Groupadd: filepath.Join(dir, "groupadd"), Groupdel: filepath.Join(dir, "groupdel"),
		IDMin: 30000, IDMax: 30999, ReservedPorts: []int{5000, 5001, 445}, MountBase: root, Timeout: 5 * time.Second,
	}, s)
	var chowned []string
	a.chown = func(p string, uid, gid int) error {
		chowned = append(chowned, p)
		return nil
	}
	return a, root, log
}

func appCall(t *testing.T, h Handler, p any) (any, error) {
	t.Helper()
	raw, _ := json.Marshal(p)
	return h(context.Background(), raw)
}

func TestAppsInstallLifecycle(t *testing.T) {
	a, root, log := testApps(t)
	res, err := appCall(t, a.CatalogOp(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := res.([]proto.CatalogApp); len(c) != 1 || c[0].ID != "homer" {
		t.Fatalf("katalog (entri tanpa digest dilewati) = %+v", c)
	}
	var oe *OpError
	for name, p := range map[string]proto.AppInstallParams{
		"port panel":       {ID: "homer", Pool: "tank", Port: 5001},
		"port istimewa":    {ID: "homer", Pool: "tank", Port: 80},
		"tidak di katalog": {ID: "jahat", Pool: "tank", Port: 8000},
		"pool aneh":        {ID: "homer", Pool: "../x", Port: 8090},
	} {
		if _, err := appCall(t, a.InstallOp(), p); !errors.As(err, &oe) {
			t.Errorf("%s harus ditolak: %v", name, err)
		}
	}
	if _, err := appCall(t, a.InstallOp(), proto.AppInstallParams{ID: "homer", Pool: "tank", Port: 8090}); err != nil {
		t.Fatal(err)
	}
	q, _ := os.ReadFile(filepath.Join(a.t.QuadletDir, "sapo-app-homer.container"))
	for _, w := range []string{
		"Image=docker.io/b4bz/homer:v25.09.1@" + testDigest, "User=30000\nGroup=30000\n", "PublishPort=8090:8080",
		"Volume=" + filepath.Join(root, "tank/sapo-apps/homer") + ":/www/assets", "Environment=TZ=Asia/Jakarta",
		"DropCapability=all\nNoNewPrivileges=true", "--memory=128m", "WantedBy=multi-user.target",
	} {
		if !strings.Contains(string(q), w) {
			t.Errorf("quadlet tanpa %q:\n%s", w, q)
		}
	}
	for _, bad := range []string{"Privileged", "Network=host", "AddDevice", "AddCapability", "podman.sock"} {
		if strings.Contains(string(q), bad) {
			t.Errorf("quadlet berisi %q", bad)
		}
	}
	calls, _ := os.ReadFile(log)
	inOrder(t, string(calls), "groupadd --system --gid 30000 -- sapo-app-homer",
		"useradd --system --uid 30000 --gid 30000 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --comment Aplikasi Sapo homer -- sapo-app-homer",
		"zfs create -o sapo:app=- -o devices=off -o setuid=off -- tank/sapo-apps",
		"zfs create -o sapo:app=homer -o devices=off -o setuid=off -o exec=on -- tank/sapo-apps/homer",
		"systemctl daemon-reload --", "systemctl start --no-block -- sapo-app-homer.service")
	// Port yang sama untuk aplikasi lain, atau memasang dua kali: ditolak.
	if _, err := appCall(t, a.InstallOp(), proto.AppInstallParams{ID: "homer", Pool: "tank", Port: 8091}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("pasang dua kali: %v", err)
	}
	res, _ = appCall(t, a.ListOp(), nil)
	if l := res.([]proto.InstalledApp); len(l) != 1 || l[0].Status != "active" || !l[0].Enabled || l[0].Port != 8090 {
		t.Fatalf("daftar = %+v", l)
	}

	// Dihentikan: tidak menyala saat boot.
	if _, err := appCall(t, a.StopOp(), proto.AppParams{ID: "homer"}); err != nil {
		t.Fatal(err)
	}
	if q, _ := os.ReadFile(filepath.Join(a.t.QuadletDir, "sapo-app-homer.container")); strings.Contains(string(q), "WantedBy") {
		t.Fatal("aplikasi yang dihentikan tidak boleh menyala saat boot")
	}

	// Dihapus, data disimpan: dipasang lagi memakai UID dan data yang sama.
	if _, err := appCall(t, a.RemoveOp(), proto.AppRemoveParams{ID: "homer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tank/sapo-apps/homer")); err != nil {
		t.Fatal("data harus disimpan")
	}
	res, _ = appCall(t, a.ListOp(), nil)
	if l := res.([]proto.InstalledApp); len(l) != 1 || l[0].Installed {
		t.Fatalf("data tersimpan = %+v", l)
	}
	if _, err := appCall(t, a.InstallOp(), proto.AppInstallParams{ID: "homer", Pool: "tank", Port: 8090}); err != nil {
		t.Fatal(err)
	}
	if q, _ := os.ReadFile(filepath.Join(a.t.QuadletDir, "sapo-app-homer.container")); !strings.Contains(string(q), "User=30000") {
		t.Fatal("pasang ulang harus memakai UID yang sama")
	}
	// Dihapus beserta data: dataset dan akun hilang, UID tidak dipakai ulang.
	if _, err := appCall(t, a.RemoveOp(), proto.AppRemoveParams{ID: "homer", RemoveData: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tank/sapo-apps/homer")); err == nil {
		t.Fatal("data harus terhapus")
	}
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "userdel -- sapo-app-homer") {
		t.Fatal("akun aplikasi harus dihapus")
	}
	if _, err := appCall(t, a.InstallOp(), proto.AppInstallParams{ID: "homer", Pool: "tank", Port: 8090}); err != nil {
		t.Fatal(err)
	}
	if q, _ := os.ReadFile(filepath.Join(a.t.QuadletDir, "sapo-app-homer.container")); !strings.Contains(string(q), "User=30001") {
		t.Fatal("UID akun yang dihapus tidak boleh dipakai ulang")
	}
}

func TestAppsRefusesTakenAccount(t *testing.T) {
	a, _, _ := testApps(t)
	// UID 30000 sudah dipakai akun lain.
	_ = os.WriteFile(a.shares.t.PasswdFile, []byte("orang:x:30000:30000::/:/bin/sh\n"), 0o644)
	var oe *OpError
	if _, err := appCall(t, a.InstallOp(), proto.AppInstallParams{ID: "homer", Pool: "tank", Port: 8090}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("UID dipakai: %v", err)
	}
}
