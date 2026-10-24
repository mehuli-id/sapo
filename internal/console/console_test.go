package console

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/audit"
	"github.com/mehuli-id/sapo/internal/auth"
	"github.com/mehuli-id/sapo/internal/config"
)

// testMenu: data panel di direktori sementara dengan satu admin ber-2FA
// dan satu pengguna biasa; systemctl hanya dicatat.
func testMenu(t *testing.T, input string) (*Menu, *bytes.Buffer, *[]string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.API.DataDir = filepath.Join(dir, "data")
	cfg.Auth.Argon2 = config.Argon2{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1}
	cfg.Agent.Console.IssueFile = filepath.Join(dir, "console.issue")
	cfg.Recovery.InterfacesFile = filepath.Join(dir, "interfaces")
	cfg.Recovery.NetDir = filepath.Join(dir, "net")
	cfg.Recovery.NetworkPendingFile = filepath.Join(dir, "network-pending.json")
	st, err := auth.OpenStore(cfg.API.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("password-lama-panjang", auth.Argon2Params{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1})
	if err := st.CompleteSetup(auth.User{Username: "arthur", Role: auth.RoleAdmin, PasswordHash: hash, TOTPSecret: "RAHASIA", RecoveryCodes: []string{"x"}}, "id", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(auth.User{Username: "budi", Role: auth.RoleUser, PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	m := NewMenu(cfg, filepath.Join(dir, "sapo.yaml"), strings.NewReader(input), out)
	calls := &[]string{}
	m.Run = func(_ context.Context, prog string, args ...string) error {
		*calls = append(*calls, filepath.Base(prog)+" "+strings.Join(args, " "))
		return nil
	}
	m.Hostname = func() (string, error) { return "sapo", nil }
	m.Addrs = func() []string { return []string{"192.168.1.10"} }
	m.Now = func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }
	m.Refresh = 0
	return m, out, calls
}

func TestResetAdmin(t *testing.T) {
	m, out, calls := testMenu(t, "1\narthur\n\n")
	_ = m.Loop(context.Background())
	pw := regexp.MustCompile(`(?m)^    ([a-z2-9]{4}(-[a-z2-9]{4}){3})$`).FindStringSubmatch(out.String())
	if pw == nil {
		t.Fatalf("password sementara tidak tampil:\n%s", out)
	}
	// Panel dihentikan sebelum data diubah, lalu dinyalakan lagi.
	if strings.Join(*calls, "; ") != "systemctl stop -- sapo-api.service; systemctl start -- sapo-api.service" {
		t.Fatalf("panggilan = %v", *calls)
	}
	st, _ := auth.OpenStore(m.Cfg.API.DataDir)
	u, _ := st.Get("arthur")
	if ok, _ := auth.VerifyPassword(pw[1], u.PasswordHash); !ok || !u.MustChangePassword || u.TOTPSecret != "" || len(u.RecoveryCodes) != 0 {
		t.Fatalf("admin setelah reset = %+v", u)
	}
	if b, _ := os.ReadFile(filepath.Join(m.Cfg.API.DataDir, "audit.log")); !strings.Contains(string(b), `"action":"recovery.reset_admin"`) || !strings.Contains(string(b), `"actor":"console"`) {
		t.Fatalf("log audit: %s", b)
	}
}

func TestResetAdminNeedsTypedName(t *testing.T) {
	m, out, calls := testMenu(t, "1\nbudi\n\n")
	_ = m.Loop(context.Background())
	if len(*calls) != 0 || !strings.Contains(out.String(), "Dibatalkan") {
		t.Fatalf("harus dibatalkan: %v\n%s", *calls, out)
	}
	// Pengguna biasa tidak bisa di-reset lewat menu.
	st, _ := auth.OpenStore(m.Cfg.API.DataDir)
	if err := st.RecoveryReset("budi", "x"); err != auth.ErrNotAdmin {
		t.Fatalf("reset pengguna biasa: %v", err)
	}
}

func TestResetNetwork(t *testing.T) {
	m, out, calls := testMenu(t, "2\nRESET\n\n")
	r := m.Cfg.Recovery
	for _, d := range []string{"eth0/device", "enp3s0/device", "lo", "docker0", "wg0"} {
		if err := os.MkdirAll(filepath.Join(r.NetDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(r.InterfacesFile, []byte("auto eth0\niface eth0 inet static\n  address 10.9.9.9/24\n"), 0o644)
	_ = os.WriteFile(r.NetworkPendingFile, []byte("{}"), 0o600)
	_ = os.WriteFile(m.AdminConfig, []byte("# catatan admin\napi:\n  listen: \":6000\"\n  allowed_networks: [10.9.0.0/16]\n  hostnames: [nas.lan]\n  audit_max_mb: 20\nauth:\n  require_totp_for_admin: false\n"), 0o644)
	_ = m.Loop(context.Background())

	want := "systemctl stop -- sapod.service; systemctl stop -- networking.service; systemctl start -- networking.service; systemctl start -- sapod.service; systemctl restart -- sapo-api.service"
	if strings.Join(*calls, "; ") != want {
		t.Fatalf("panggilan = %v\n%s", *calls, out)
	}
	if _, err := os.Stat(r.NetworkPendingFile); err == nil {
		t.Fatal("perubahan jaringan yang menunggu konfirmasi harus dibuang")
	}
	ifs, _ := os.ReadFile(r.InterfacesFile)
	for _, w := range []string{"auto lo\niface lo inet loopback", "auto enp3s0\niface enp3s0 inet dhcp", "auto eth0\niface eth0 inet dhcp\niface eth0 inet6 auto"} {
		if !strings.Contains(string(ifs), w) {
			t.Errorf("interfaces tanpa %q:\n%s", w, ifs)
		}
	}
	if strings.Contains(string(ifs), "docker0") || strings.Contains(string(ifs), "wg0") || strings.Contains(string(ifs), "static") {
		t.Fatalf("hanya antarmuka fisik dengan DHCP:\n%s", ifs)
	}
	if b, _ := os.ReadFile(r.InterfacesFile + ".sapo-20261024-120000"); !strings.Contains(string(b), "10.9.9.9") {
		t.Fatalf("cadangan interfaces: %q", b)
	}
	conf, _ := os.ReadFile(m.AdminConfig)
	c := string(conf)
	for _, gone := range []string{"listen", "allowed_networks", "hostnames"} {
		if strings.Contains(c, gone) {
			t.Errorf("%s harus kembali ke bawaan:\n%s", gone, c)
		}
	}
	for _, kept := range []string{"# catatan admin", "audit_max_mb: 20", "require_totp_for_admin: false"} {
		if !strings.Contains(c, kept) {
			t.Errorf("%q harus tetap:\n%s", kept, c)
		}
	}
	if _, err := config.LoadLayered("", m.AdminConfig); err != nil {
		t.Fatalf("konfigurasi baru tidak valid: %v", err)
	}
}

func TestPowerNeedsConfirmation(t *testing.T) {
	m, _, calls := testMenu(t, "3\ntidak\n\n4\ny\n")
	_ = m.Loop(context.Background())
	if strings.Join(*calls, "; ") != "systemctl poweroff" {
		t.Fatalf("panggilan = %v", *calls)
	}
}

func TestScreenShowsPanelAddress(t *testing.T) {
	m, out, _ := testMenu(t, "")
	_ = os.WriteFile(m.Cfg.Agent.Console.IssueFile, []byte("Panel Sapo: https://\\4:5001  atau  https://\\n.local:5001\n\033[31m"), 0o644)
	_ = m.Loop(context.Background())
	s := out.String()
	if !strings.Contains(s, "https://192.168.1.10:5001  atau  https://sapo.local:5001") || strings.Contains(s, "\033[31m") {
		t.Fatalf("layar:\n%q", s)
	}
}

func TestScreenRefreshesWhileIdle(t *testing.T) {
	pr, pw, _ := os.Pipe()
	defer pw.Close()
	m, out, _ := testMenu(t, "")
	m.In, m.Refresh = bufio.NewReader(pr), 20*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = m.Loop(ctx)
	if n := strings.Count(out.String(), "menu darurat konsol"); n < 3 {
		t.Fatalf("layar harus digambar ulang saat menganggur, %d kali", n)
	}
}

func TestHelperMain(t *testing.T) {
	m, _, _ := testMenu(t, "")
	dir := m.Cfg.API.DataDir
	var out, errOut bytes.Buffer
	if code := HelperMain([]string{"-data", dir, "admins"}, strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), `"admins":["arthur"]`) {
		t.Fatalf("admins: %d %s %s", code, out.String(), errOut.String())
	}
	if code := HelperMain([]string{"-data", dir, "reset"}, strings.NewReader(`{"name":"budi","hash":"x"}`), &out, &errOut); code == 0 {
		t.Fatal("pengguna biasa tidak boleh di-reset")
	}
	if code := HelperMain([]string{"-data", dir, "reset"}, strings.NewReader(`{"name":"arthur","hash":"h"}`), &out, &errOut); code != 0 {
		t.Fatalf("reset: %s", errOut.String())
	}
	st, _ := auth.OpenStore(dir)
	if u, _ := st.Get("arthur"); u.PasswordHash != "h" || !u.MustChangePassword {
		t.Fatalf("admin = %+v", u)
	}
	if code := HelperMain([]string{"-data", dir, "hapus"}, strings.NewReader(""), &out, &errOut); code == 0 {
		t.Fatal("perintah tidak dikenal harus ditolak")
	}
}

func TestResetAdminSkipsUnsafeNames(t *testing.T) {
	m, out, calls := testMenu(t, "1\n")
	m.Data = fakeData{admins: []string{"\033[2Jjahat"}}
	_ = m.Loop(context.Background())
	if strings.Contains(out.String(), "\033[2Jjahat") || len(*calls) != 0 || !strings.Contains(out.String(), "Belum ada administrator") {
		t.Fatalf("nama tidak sah harus dilewati:\n%q", out)
	}
}

type fakeData struct{ admins []string }

func (f fakeData) Admins() ([]string, string, error) { return f.admins, "id", nil }
func (f fakeData) ResetAdmin(string, string) error   { return nil }
func (f fakeData) Audit(audit.Event)                 {}

func TestResetAPIKeysMovesBrokenFileAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sapo.yaml")
	for name, body := range map[string]string{
		"rusak":         "api:\n  allowed_networks: [10.0.0.0/8\n",
		"alias":         "x: &a\n  listen: \":1\"\napi: *a\n",
		"dua dokumen":   "api:\n  audit_max_mb: 5\n---\napi:\n  listen: \":1\"\n",
		"bukan mapping": "- satu\n",
	} {
		_ = os.WriteFile(path, []byte(body), 0o640)
		backup := filepath.Join(dir, name+".bak")
		if err := resetAPIKeys(path, backup, []string{"listen"}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s: file harus dipindah", name)
		}
		if b, _ := os.ReadFile(backup); string(b) != body {
			t.Errorf("%s: cadangan = %q", name, b)
		}
	}
}

func TestBackupKeepsModeAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "interfaces")
	_ = os.WriteFile(path, []byte("wpa-psk rahasia\n"), 0o600)
	backup := path + ".sapo-1"
	if err := backupAndWrite(path, backup, []byte("baru\n")); err != nil {
		t.Fatal(err)
	}
	if err := backupAndWrite(path, backup, []byte("baru lagi\n")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, backup, backup + "-2"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
	if b, _ := os.ReadFile(backup); string(b) != "wpa-psk rahasia\n" {
		t.Fatalf("cadangan pertama tertimpa: %q", b)
	}
}

func TestSecretClearedAfterTimeout(t *testing.T) {
	pr, pw, _ := os.Pipe()
	defer pw.Close()
	m, out, _ := testMenu(t, "")
	m.In, m.SecretTimeout = bufio.NewReader(pr), 50*time.Millisecond
	go func() { _, _ = pw.WriteString("1\narthur\n") }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = m.Loop(ctx)
	s := out.String()
	i := strings.Index(s, "Password sementara")
	if i < 0 || !strings.Contains(s[i:], "\033[2J") {
		t.Fatalf("layar harus dibersihkan setelah password tampil:\n%q", s)
	}
}
