package agent

import (
	"os"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Bagian Go dari uji Wi-Fi sungguhan (tools/wifi-hwsim/run.sh, ADR 0023):
// hanya berjalan bila variabel SAPO_* diisi skrip itu.

// TestHwsimWPAConf menulis file wpa_supplicant buatan sapod ke
// SAPO_WPA_CONF_OUT untuk SAPO_WIFI_SSID dan SAPO_WIFI_PASSWORD.
func TestHwsimWPAConf(t *testing.T) {
	out := os.Getenv("SAPO_WPA_CONF_OUT")
	if out == "" {
		t.Skip("hanya dari tools/wifi-hwsim/run.sh")
	}
	w := proto.WiFiNetwork{SSID: os.Getenv("SAPO_WIFI_SSID"), Password: os.Getenv("SAPO_WIFI_PASSWORD")}
	if err := checkWiFi(&w); err != nil {
		t.Fatal(err)
	}
	conf, err := renderWPAConf("ID", w)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, conf, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestHwsimIw membaca keluaran iw sungguhan: hasil pindai (SAPO_IW_SCAN)
// harus memuat SAPO_WIFI_SSID sebagai jaringan yang bisa dipakai, dan
// keadaan sambungan (SAPO_IW_LINK) harus tersambung ke SSID itu.
func TestHwsimIw(t *testing.T) {
	scan, link, ssid := os.Getenv("SAPO_IW_SCAN"), os.Getenv("SAPO_IW_LINK"), os.Getenv("SAPO_WIFI_SSID")
	if scan == "" {
		t.Skip("hanya dari tools/wifi-hwsim/run.sh")
	}
	data, err := os.ReadFile(scan)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range parseIwScan(data) {
		if s.SSID == ssid && s.Secure && s.SignalDBm < 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("%q tidak ada di hasil pindai: %+v\n%s", ssid, parseIwScan(data), data)
	}
	if link == "" {
		return
	}
	data, err = os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := parseIwLink(data); got != ssid {
		t.Fatalf("tersambung ke %q, harus %q:\n%s", got, ssid, data)
	}
}
