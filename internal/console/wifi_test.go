package console

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// fakeAgent: sapod tiruan untuk Wi-Fi. connect: setelah apply, wlan0
// tersambung dan beralamat.
type fakeAgent struct {
	mu      sync.Mutex
	connect bool
	applied *proto.NetworkConfig
	ops     []string
}

func (f *fakeAgent) Call(_ context.Context, op string, params any, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
	var res any
	switch op {
	case proto.OpNetworkStatus:
		wlan := proto.NetworkLink{Name: "wlan0", Wireless: true, Addresses: []string{}}
		if f.applied != nil && f.connect {
			wlan.SSID, wlan.Addresses = f.applied.Interfaces[1].WiFi.SSID, []string{"192.168.1.77/24", "fd00::7/64"}
		}
		res = proto.NetworkStatus{Hostname: "sapo", Interfaces: []proto.NetworkLink{{Name: "eth0", Addresses: []string{}}, wlan}}
	case proto.OpNetworkWiFiScan:
		res = []proto.WiFiSeen{{SSID: "Rumah", SignalDBm: -50, Secure: true}, {SSID: "Kafe", SignalDBm: -40}, {SSID: "Tetangga", SignalDBm: -80, Secure: true}}
	case proto.OpNetworkApply:
		c := params.(proto.NetworkApplyParams).Config
		f.applied = &c
		res = proto.NetworkApplyResult{Token: strings.Repeat("a", 64)}
	case proto.OpNetworkConfirm, proto.OpNetworkCancel:
		res = map[string]bool{"ok": true}
	}
	if out != nil {
		b, _ := json.Marshal(res)
		return json.Unmarshal(b, out)
	}
	return nil
}

func wifiMenu(t *testing.T, input string, connect bool) (*Menu, *fakeAgent, *[]bool, *strings.Builder) {
	t.Helper()
	m, out, _ := testMenu(t, input)
	f := &fakeAgent{connect: connect}
	m.Agent = f
	echo := &[]bool{}
	m.Echo = func(on bool) { *echo = append(*echo, on) }
	m.Sleep = func(time.Duration) {}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { now = now.Add(10 * time.Second); return now }
	sb := &strings.Builder{}
	_ = m.Loop(context.Background())
	sb.WriteString(out.String())
	return m, f, echo, sb
}

func TestConsoleWiFi(t *testing.T) {
	// Jaringan nomor 1 (yang aman saja yang bernomor), password berspasi.
	m, f, echo, out := wifiMenu(t, "5\n1\n  kata sandi rumah \n\n", true)
	if f.applied == nil {
		t.Fatalf("tidak ada apply:\n%s", out)
	}
	c := *f.applied
	if c.Hostname != "sapo" || len(c.Interfaces) != 2 || c.Interfaces[0].Mode != "dhcp" || c.Interfaces[1].WiFi == nil ||
		c.Interfaces[1].WiFi.SSID != "Rumah" || c.Interfaces[1].WiFi.Password != "  kata sandi rumah " || c.Interfaces[1].Mode != "dhcp" {
		t.Fatalf("pengaturan = %+v", c)
	}
	if strings.Contains(out.String(), "kata sandi") {
		t.Fatal("password tidak boleh tampil di layar")
	}
	if len(*echo) != 2 || (*echo)[0] || !(*echo)[1] {
		t.Fatalf("gema keyboard = %v", *echo)
	}
	if !strings.Contains(out.String(), "Tersambung ke Rumah. Alamat NAS: 192.168.1.77") || !strings.Contains(strings.Join(f.ops, " "), proto.OpNetworkConfirm) {
		t.Fatalf("keluaran:\n%s\nops %v", out, f.ops)
	}
	if strings.Contains(out.String(), "Kafe") {
		t.Fatal("jaringan terbuka tidak boleh ditawarkan")
	}
	if b, _ := os.ReadFile(filepath.Join(m.Cfg.API.DataDir, "audit.log")); !strings.Contains(string(b), `"action":"recovery.wifi"`) || strings.Contains(string(b), "kata sandi") {
		t.Fatalf("log audit: %s", b)
	}
}

func TestConsoleWiFiFails(t *testing.T) {
	// Jaringan tersembunyi diketik sendiri; tidak tersambung: dibatalkan.
	_, f, _, out := wifiMenu(t, "5\n0\nRahasia\nsalah-password\n\n", false)
	ops := strings.Join(f.ops, " ")
	if f.applied == nil || f.applied.Interfaces[1].WiFi.SSID != "Rahasia" || !strings.Contains(ops, proto.OpNetworkCancel) || strings.Contains(ops, proto.OpNetworkConfirm) {
		t.Fatalf("ops %v\n%s", f.ops, out)
	}
	if !strings.Contains(out.String(), "Tidak bisa tersambung ke Rahasia") {
		t.Fatalf("keluaran:\n%s", out)
	}
}

func TestResetNetworkLeavesWiFiOff(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "wlan0/wireless"), 0o755)
	got := string(renderInterfaces([]string{"eth0", "wlan0"}, wirelessIn(dir), "x"))
	if !strings.Contains(got, "auto eth0\niface eth0 inet dhcp") || !strings.Contains(got, "iface wlan0 inet manual") || strings.Contains(got, "auto wlan0") {
		t.Fatalf("interfaces:\n%s", got)
	}
}
