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
	// useradd/groupadd menambah baris ke passwd/group tiruan; userdel/
	// groupdel membuangnya.
	passwd, group := s.t.PasswdFile, s.t.GroupFile
	add := `for a; do last="$a"; done; id=""; prev=""; for a; do case "$prev" in --uid|--gid) id="$a" ;; esac; prev="$a"; done; `
	for _, name := range []string{"useradd", "userdel", "groupadd", "groupdel", "systemctl"} {
		script := "#!/bin/sh\nprintf '%s %s\\n' " + name + " \"$*\" >> '" + log + "'\n"
		switch name {
		case "systemctl":
			script += "case \"$1\" in is-active) echo active ;; esac\n"
		case "useradd":
			script += add + "echo \"$last:x:$id:$id::/nonexistent:/usr/sbin/nologin\" >> '" + passwd + "'\n"
		case "groupadd":
			script += add + "echo \"$last:x:$id:\" >> '" + group + "'\n"
		case "userdel":
			script += "for a; do last=\"$a\"; done; grep -v \"^$last:\" '" + passwd + "' > '" + passwd + ".n'; mv '" + passwd + ".n' '" + passwd + "'\n"
		case "groupdel":
			script += "for a; do last=\"$a\"; done; grep -v \"^$last:\" '" + group + "' > '" + group + ".n'; mv '" + group + ".n' '" + group + "'\n"
		}
		_ = os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755)
	}
	a := NewApps(AppsTools{
		CatalogDir: cat, Podman: "/usr/bin/podman", QuadletDir: filepath.Join(dir, "quadlet"), StateFile: filepath.Join(dir, "apps.json"),
		Systemctl: filepath.Join(dir, "systemctl"), Useradd: filepath.Join(dir, "useradd"), Userdel: filepath.Join(dir, "userdel"),
		Groupadd: filepath.Join(dir, "groupadd"), Groupdel: filepath.Join(dir, "groupdel"),
		IDMin: 30000, IDMax: 30999, ReservedPorts: []int{5000, 5001, 445}, MountBase: root, Timeout: 5 * time.Second,
	}, s)
	a.portFree = func(int) bool { return true }
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

func TestAppsGuards(t *testing.T) {
	a, _, log := testApps(t)
	if _, err := appCall(t, a.InstallOp(), proto.AppInstallParams{ID: "homer", Pool: "tank", Port: 8090}); err != nil {
		t.Fatal(err)
	}
	// Pool berisi data aplikasi tidak boleh dihapus.
	var oe *OpError
	if err := a.shares.p.beforeDestroy[0](context.Background(), "tank"); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("hapus pool berisi data aplikasi: %v", err)
	}
	// Katalog membawa digest baru: file Quadlet ditulis ulang dan aplikasi
	// di-restart saat sapod menyala.
	cat := filepath.Join(a.t.CatalogDir, "homer.yaml")
	b, _ := os.ReadFile(cat)
	_ = os.WriteFile(cat, []byte(strings.Replace(string(b), testDigest, "sha256:"+strings.Repeat("a", 64), 1)), 0o644)
	a.Reconcile(context.Background())
	if q, _ := os.ReadFile(a.quadletPath("homer")); !strings.Contains(string(q), strings.Repeat("a", 64)) {
		t.Fatalf("quadlet tidak diperbarui:\n%s", q)
	}
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "systemctl try-restart -- sapo-app-homer.service") {
		t.Fatal("aplikasi yang berubah harus di-restart")
	}
	// Port yang sudah dipakai program lain.
	a.portFree = func(int) bool { return false }
	if _, err := appCall(t, a.RemoveOp(), proto.AppRemoveParams{ID: "homer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := appCall(t, a.InstallOp(), proto.AppInstallParams{ID: "homer", Pool: "tank", Port: 8090}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("port dipakai: %v", err)
	}
}

func TestAppsShares(t *testing.T) {
	a, root, log := testApps(t)
	_ = os.WriteFile(filepath.Join(a.t.CatalogDir, "galeri.yaml"), []byte(`id: galeri
name: { id: Galeri, en: Gallery }
description: { id: Uji, en: Test }
image: docker.io/x/galeri:1@`+testDigest+`
web_port: 8080
default_port: 8091
data_path: /data
share_path: /folder
memory_mb: 64
`), 0o644)
	for _, p := range []proto.AppInstallParams{{ID: "homer", Pool: "tank", Port: 8090}, {ID: "galeri", Pool: "tank", Port: 8091}} {
		if _, err := appCall(t, a.InstallOp(), p); err != nil {
			t.Fatal(err)
		}
	}
	s := a.shares
	// Galeri diberi film (minta tulis, katalog hanya baca); homer tidak
	// memakai shared folder; pribadi tidak diberikan ke aplikasi mana pun.
	_ = os.Truncate(log, 0)
	if _, err := applyShares(t, s,
		proto.ShareSpec{Name: "film", Pool: "tank", Read: []string{"user:ani"}, Write: []string{}, Apps: []proto.ShareApp{{ID: "galeri", Write: true}, {ID: "homer"}, {ID: "hantu"}}},
		proto.ShareSpec{Name: "pribadi", Pool: "tank", Read: []string{}, Write: []string{"user:budi"}},
	); err != nil {
		t.Fatal(err)
	}
	q, _ := os.ReadFile(a.quadletPath("galeri"))
	film := filepath.Join(root, "tank/film")
	if !strings.Contains(string(q), "Volume="+film+":/folder/film:ro,nosuid,nodev,noexec\n") || !strings.Contains(string(q), "RequiresMountsFor="+film+"\n") {
		t.Fatalf("film harus dipasang baca saja:\n%s", q)
	}
	if strings.Contains(string(q), "pribadi") {
		t.Fatalf("folder yang tidak diberikan ikut dipasang:\n%s", q)
	}
	if q, _ := os.ReadFile(a.quadletPath("homer")); strings.Contains(string(q), "film") {
		t.Fatalf("aplikasi tanpa share_path tidak boleh mendapat folder:\n%s", q)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "u:sapo-app-galeri:r-x") || strings.Contains(string(calls), "sapo-app-homer:") || strings.Contains(string(calls), "hantu") {
		t.Fatalf("ACL akun aplikasi salah:\n%s", calls)
	}
	inOrder(t, string(calls), "systemctl daemon-reload --", "systemctl try-restart --no-block -- sapo-app-galeri.service")
	if conf, _ := os.ReadFile(s.t.SMBConf); strings.Contains(string(conf), "sapo-app") {
		t.Fatalf("akun aplikasi tidak boleh masuk smb.conf:\n%s", conf)
	}
	res, _ := appCall(t, a.ListOp(), nil)
	for _, app := range res.([]proto.InstalledApp) {
		if app.ID == "galeri" && (len(app.Shares) != 1 || app.Shares[0] != (proto.AppShare{Name: "film"})) {
			t.Fatalf("folder galeri = %+v", app.Shares)
		}
	}
	// Folder dan pool yang dipakai aplikasi tidak bisa dihapus.
	var oe *OpError
	if _, err := appCall(t, s.DeleteOp(), proto.ShareDeleteParams{Name: "film", Pool: "tank"}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict || !strings.Contains(oe.Message, "galeri") {
		t.Fatalf("hapus folder yang dipakai aplikasi: %v", err)
	}
	// Penerapan yang sama tidak me-restart lagi.
	_ = os.Truncate(log, 0)
	spec := proto.ShareSpec{Name: "film", Pool: "tank", Read: []string{"user:ani"}, Write: []string{}, Apps: []proto.ShareApp{{ID: "galeri"}}}
	if _, err := applyShares(t, s, spec); err != nil {
		t.Fatal(err)
	}
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "try-restart") {
		t.Fatalf("tanpa perubahan tidak boleh restart:\n%s", calls)
	}
	// Dicabut: folder dilepas dari container dan ACL akun aplikasi hilang.
	spec.Apps = nil
	_ = os.Truncate(log, 0)
	if _, err := applyShares(t, s, spec); err != nil {
		t.Fatal(err)
	}
	if q, _ := os.ReadFile(a.quadletPath("galeri")); strings.Contains(string(q), "film") {
		t.Fatalf("folder yang dicabut masih dipasang:\n%s", q)
	}
	calls, _ = os.ReadFile(log)
	if strings.Contains(string(calls), "sapo-app-galeri:") {
		t.Fatalf("ACL aplikasi masih ada:\n%s", calls)
	}
	if i, j := strings.Index(string(calls), "systemctl stop"), strings.Index(string(calls), "setfacl"); i < 0 || i > j {
		t.Fatalf("aplikasi harus dihentikan sebelum ACL diubah:\n%s", calls)
	}
	// Dihentikan sebelum ACL diubah, lalu dijalankan lagi tanpa folder itu.
	inOrder(t, string(calls), "systemctl stop -- sapo-app-galeri.service", "smbcontrol smbd reload-config --", "systemctl start --no-block -- sapo-app-galeri.service")
	if id := a.usingShare(film); id != "" {
		t.Fatalf("folder masih tercatat dipakai %s", id)
	}
	// systemctl gagal: catatan tidak berubah, jadi sinkronisasi ulang
	// mencoba lagi.
	good, _ := os.ReadFile(a.t.Systemctl)
	_ = os.WriteFile(a.t.Systemctl, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	spec.Apps = []proto.ShareApp{{ID: "galeri"}}
	if _, err := applyShares(t, s, spec); err == nil {
		t.Fatal("systemctl gagal harus dilaporkan")
	}
	if id := a.usingShare(film); id != "" {
		t.Fatalf("catatan tidak boleh berubah saat gagal: %s", id)
	}
	_ = os.WriteFile(a.t.Systemctl, good, 0o755)
	if _, err := applyShares(t, s, spec); err != nil {
		t.Fatal(err)
	}
	if id := a.usingShare(film); id != "galeri" {
		t.Fatalf("percobaan ulang harus menerapkan: %q", id)
	}
}

func TestAppCatalogSharePath(t *testing.T) {
	base := appDef{ID: "x1", Image: "docker.io/x/y:1@" + testDigest, WebPort: 80, DefaultPort: 8000, DataPath: "/data", MemoryMB: 64}
	for name, mod := range map[string]func(*appDef){
		"bertumpuk":      func(d *appDef) { d.SharePath = "/data/media" },
		"induk data":     func(d *appDef) { d.SharePath = "/" + "d" },
		"relatif":        func(d *appDef) { d.SharePath = "media" },
		"tulis saja":     func(d *appDef) { d.ShareWrite = true },
		"id kepanjangan": func(d *appDef) { d.ID = "a" + strings.Repeat("b", 23) },
	} {
		d := base
		mod(&d)
		if name == "induk data" {
			d.DataPath, d.SharePath = "/srv/data", "/srv"
		}
		if d.validate() == nil {
			t.Errorf("%s harus ditolak", name)
		}
	}
	d := base
	d.SharePath, d.ShareWrite = "/media", true
	if err := d.validate(); err != nil {
		t.Fatal(err)
	}
}

// Semua entri katalog di repo harus valid (entri tidak valid dilewati diam-
// diam saat berjalan).
func TestRepoCatalogValid(t *testing.T) {
	entries, err := os.ReadDir("../../apps")
	if err != nil {
		t.Fatal(err)
	}
	a := &Apps{t: AppsTools{CatalogDir: "../../apps"}}
	cat, err := a.catalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(cat) != len(entries) || len(cat) == 0 {
		t.Fatalf("katalog valid %d dari %d file", len(cat), len(entries))
	}
}

func TestSharesRejectAppPrincipals(t *testing.T) {
	s, _, _ := testShares(t, "0")
	var oe *OpError
	for _, p := range []string{"user:sapo-app-homer", "group:sapo-app-homer"} {
		if _, err := applyShares(t, s, proto.ShareSpec{Name: "film", Pool: "tank", Read: []string{p}, Write: []string{}}); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
			t.Errorf("%s di izin biasa harus ditolak: %v", p, err)
		}
	}
}
