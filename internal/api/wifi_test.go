package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestWiFiAPI(t *testing.T) {
	agent := shareAgent()
	agent.results[proto.OpNetworkApply] = proto.NetworkApplyResult{Token: strings.Repeat("a", 64)}
	agent.results[proto.OpNetworkWiFiScan] = []proto.WiFiSeen{{SSID: "Rumah", SignalDBm: -50, Secure: true}}
	h := newHarness(t, agent)
	h.runSetup()
	body := map[string]any{"admin_password": adminPassword, "config": map[string]any{"hostname": "nas", "dns": []string{},
		"interfaces": []any{map[string]any{"name": "wlan0", "mode": "dhcp", "wifi": map[string]string{"ssid": "Rumah", "password": "rahasia-wifi-rumah"}}}}}
	if rec := h.do(http.MethodPost, "/api/v1/network", body); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	// Password diteruskan ke sapod, tetapi tidak masuk log audit.
	if p, _ := agent.param(proto.OpNetworkApply).(proto.NetworkApplyParams); p.Config.Interfaces[0].WiFi.Password != "rahasia-wifi-rumah" {
		t.Fatalf("network.apply = %+v", p)
	}
	logs, _ := os.ReadFile(filepath.Join(h.data, "audit.log"))
	if !strings.Contains(string(logs), "network.apply") || !strings.Contains(string(logs), "Rumah") || strings.Contains(string(logs), "rahasia-wifi-rumah") {
		t.Fatalf("audit:\n%s", logs)
	}
	if rec := h.do(http.MethodPost, "/api/v1/network/wifi-scan", map[string]string{"interface": "wlan0"}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Rumah") {
		t.Fatalf("pindai: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpNetworkWiFiScan).(proto.WiFiScanParams); p.Interface != "wlan0" {
		t.Fatalf("wifi_scan = %+v", p)
	}
	h.cookie = nil
	if rec := h.do(http.MethodPost, "/api/v1/network/wifi-scan", map[string]string{"interface": "wlan0"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("pindai tanpa login: %d", rec.Code)
	}
}
