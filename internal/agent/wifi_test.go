package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

const iwScanSample = `BSS 11:22:33:44:55:66(on wlan0)
	freq: 2437
	signal: -48.00 dBm
	SSID: Rumah
	RSN:	 * Version: 1
		 * Group cipher: CCMP
		 * Pairwise ciphers: CCMP
		 * Authentication suites: PSK SAE
BSS 11:22:33:44:55:67(on wlan0)
	signal: -70.00 dBm
	SSID: Rumah
	RSN:	 * Version: 1
		 * Authentication suites: PSK
BSS aa:bb:cc:dd:ee:ff(on wlan0)
	signal: -60.00 dBm
	SSID: Kafe\x20Gratis
BSS aa:bb:cc:dd:ee:00(on wlan0)
	signal: -55.00 dBm
	SSID: Kantor
	RSN:	 * Version: 1
		 * Authentication suites: IEEE 802.1X
BSS aa:bb:cc:dd:ee:01(on wlan0)
	signal: -40.00 dBm
	SSID: 
	RSN:	 * Authentication suites: PSK
BSS aa:bb:cc:dd:ee:02(on wlan0)
	signal: -65.00 dBm
	SSID: Lama
	WPA:	 * Version: 1
		 * Authentication suites: PSK
`

// testWiFiNetwork: testNetwork ditambah antarmuka Wi-Fi wlan0.
func testWiFiNetwork(t *testing.T) (*Network, string, *netCalls) {
	t.Helper()
	n, dir, calls := testNetwork(t)
	for _, d := range []string{"net/wlan0/device", "net/wlan0/wireless", "wpa"} {
		_ = os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	n.t.Iw, n.t.WPAConfDir, n.t.WiFiCountry = "/usr/sbin/iw", filepath.Join(dir, "wpa"), "ID"
	n.runArgs = func(_ context.Context, prog string, args ...string) (toolResult, error) {
		calls.add(filepath.Base(prog) + " " + strings.Join(args, " "))
		switch strings.Join(args, " ") {
		case "dev wlan0 scan":
			return toolResult{Stdout: []byte(iwScanSample)}, nil
		case "dev wlan0 link":
			return toolResult{Stdout: []byte("Connected to 11:22:33:44:55:66 (on wlan0)\n\tSSID: Rumah\n\tfreq: 2437\n\tsignal: -48 dBm\n")}, nil
		}
		return toolResult{ExitCode: 1}, nil
	}
	return n, dir, calls
}

func TestParseIwScan(t *testing.T) {
	got := parseIwScan([]byte(iwScanSample))
	want := []proto.WiFiSeen{{SSID: "Rumah", SignalDBm: -48, Secure: true}, {SSID: "Kantor", SignalDBm: -55, Secure: false},
		{SSID: "Kafe Gratis", SignalDBm: -60, Secure: false}, {SSID: "Lama", SignalDBm: -65, Secure: true}}
	if len(got) != len(want) {
		t.Fatalf("hasil = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, harus %+v", i, got[i], want[i])
		}
	}
}

func TestRenderWPAConf(t *testing.T) {
	// Vektor uji IEEE 802.11i: SSID "IEEE", passphrase "password".
	conf, err := renderWPAConf("ID", proto.WiFiNetwork{SSID: "IEEE", Password: "password"})
	if err != nil {
		t.Fatal(err)
	}
	c := string(conf)
	for _, want := range []string{"country=ID\n", "ssid=" + hex.EncodeToString([]byte("IEEE")) + "\n",
		"psk=f42c6fc52df0ebef9ebb4b90b38a5f902e83fe1b135a70e23aed762e9710a12e\n", "sae_password=" + hex.EncodeToString([]byte("password")) + "\n", "key_mgmt=WPA-PSK WPA-PSK-SHA256 SAE\n"} {
		if !strings.Contains(c, want) {
			t.Errorf("tanpa %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "=password") || strings.Contains(c, `"`) {
		t.Fatalf("password atau tanda kutip tertulis apa adanya:\n%s", c)
	}
	if _, err := renderWPAConf("id\n", proto.WiFiNetwork{SSID: "x", Password: "12345678"}); err == nil {
		t.Fatal("kode negara aneh harus ditolak")
	}
}

func TestWiFiCheckConfig(t *testing.T) {
	n, _, _ := testWiFiNetwork(t)
	wifi := func(w *proto.WiFiNetwork) proto.NetworkConfig {
		return proto.NetworkConfig{Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "dhcp"}, {Name: "wlan0", Mode: "dhcp", WiFi: w}}}
	}
	// Wi-Fi yang tidak disebut: tidak dipakai.
	got, err := n.checkConfig(proto.NetworkConfig{Hostname: "sapo"}, proto.NetworkConfig{})
	if err != nil || got.Interfaces[2].Name != "wlan0" || got.Interfaces[2].Mode != "off" {
		t.Fatalf("%v %+v", err, got)
	}
	for name, c := range map[string]proto.NetworkConfig{
		"tanpa jaringan":     wifi(nil),
		"password pendek":    wifi(&proto.WiFiNetwork{SSID: "Rumah", Password: "1234"}),
		"SSID baris baru":    wifi(&proto.WiFiNetwork{SSID: "a\nnetwork={", Password: "12345678"}),
		"SSID kepanjangan":   wifi(&proto.WiFiNetwork{SSID: strings.Repeat("x", 33), Password: "12345678"}),
		"password non-ASCII": wifi(&proto.WiFiNetwork{SSID: "Rumah", Password: "kata sandi é"}),
		"password kosong":    wifi(&proto.WiFiNetwork{SSID: "Rumah"}),
		"Wi-Fi di kabel":     {Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "dhcp", WiFi: &proto.WiFiNetwork{SSID: "x", Password: "12345678"}}}},
	} {
		if _, err := n.checkConfig(c, proto.NetworkConfig{}); err == nil {
			t.Errorf("%s harus ditolak", name)
		}
	}
	// Password kosong: dari pengaturan terakhir bila SSID sama.
	prev := wifi(&proto.WiFiNetwork{SSID: "Rumah", Password: "rahasia-rumah"})
	got, err = n.checkConfig(wifi(&proto.WiFiNetwork{SSID: "Rumah"}), prev)
	if err != nil || got.Interfaces[2].WiFi.Password != "rahasia-rumah" {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := n.checkConfig(wifi(&proto.WiFiNetwork{SSID: "Lain"}), prev); err == nil {
		t.Fatal("SSID lain tanpa password harus ditolak")
	}
}

func TestWiFiApply(t *testing.T) {
	n, dir, _ := testWiFiNetwork(t)
	cfg := proto.NetworkConfig{Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "dhcp"}, {Name: "eth1", Mode: "off"},
		{Name: "wlan0", Mode: "dhcp", WiFi: &proto.WiFiNetwork{SSID: `Rumah "Kita"`, Password: "rahasia-rumah"}}}}
	res, err := call(t, n.ApplyOp(), proto.NetworkApplyParams{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "wpa/sapo-wlan0.conf")
	waitFor(t, func() bool { _, err := os.Stat(conf); return err == nil })
	fi, _ := os.Stat(conf)
	data, _ := os.ReadFile(conf)
	if fi.Mode().Perm() != 0o600 || strings.Contains(string(data), "rahasia") || !strings.Contains(string(data), hex.EncodeToString([]byte(`Rumah "Kita"`))) {
		t.Fatalf("file wpa (%v):\n%s", fi.Mode().Perm(), data)
	}
	ifaces, _ := os.ReadFile(filepath.Join(dir, "etc/interfaces"))
	if !strings.Contains(string(ifaces), "iface wlan0 inet dhcp\n    wpa-conf "+conf+"\n") {
		t.Fatalf("interfaces:\n%s", ifaces)
	}
	waitFor(t, func() bool {
		_, err := call(t, n.ConfirmOp(), proto.NetworkTokenParams{Token: res.(proto.NetworkApplyResult).Token})
		return err == nil
	})
	// Status: password tidak pernah keluar; sambungan Wi-Fi terlihat.
	st, err := call(t, n.StatusOp(), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), "rahasia") {
		t.Fatalf("password bocor di status: %s", raw)
	}
	s := st.(proto.NetworkStatus)
	var wlan proto.NetworkLink
	for _, l := range s.Interfaces {
		if l.Name == "wlan0" {
			wlan = l
		}
	}
	if !wlan.Wireless || wlan.SSID != "Rumah" || wlan.SignalDBm != -48 || s.Config.Interfaces[2].WiFi.SSID != `Rumah "Kita"` {
		t.Fatalf("status = %+v", s)
	}
	// Wi-Fi dimatikan lalu dibatalkan: file wpa (dengan mode 0600) kembali.
	cfg.Interfaces[2] = proto.NetworkInterface{Name: "wlan0", Mode: "off"}
	res, err = call(t, n.ApplyOp(), proto.NetworkApplyParams{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(conf); return os.IsNotExist(err) })
	waitFor(t, func() bool {
		_, err := call(t, n.CancelOp(), proto.NetworkTokenParams{Token: res.(proto.NetworkApplyResult).Token})
		return err == nil
	})
	if fi, err := os.Stat(conf); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file wpa harus kembali dengan mode 0600: %v", err)
	}
}

func TestWiFiScan(t *testing.T) {
	n, _, calls := testWiFiNetwork(t)
	out, err := call(t, n.WiFiScanOp(), proto.WiFiScanParams{Interface: "wlan0"})
	if err != nil || len(out.([]proto.WiFiSeen)) != 4 {
		t.Fatalf("%v %+v", err, out)
	}
	if !strings.Contains(calls.String(), "ip link set dev wlan0 up") {
		t.Fatalf("antarmuka harus dinyalakan dulu:\n%s", calls)
	}
	var oe *OpError
	for _, bad := range []string{"eth0", "-h", "docker0", "tidakada"} {
		if _, err := call(t, n.WiFiScanOp(), proto.WiFiScanParams{Interface: bad}); err == nil {
			t.Errorf("%q harus ditolak", bad)
		} else if !strings.Contains(err.Error(), "Wi-Fi") && !errorsAs(err, &oe) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func errorsAs(err error, oe **OpError) bool {
	e, ok := err.(*OpError)
	*oe = e
	return ok
}
