package console

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// --- Sambungkan Wi-Fi (ADR 0023) ---------------------------------------------
//
// Untuk komputer tanpa kabel LAN: panel belum bisa dibuka sebelum Wi-Fi
// tersambung. Menu memakai operasi network.* milik sapod yang sama dengan
// panel (validasi, file wpa_supplicant 0600, pengembalian otomatis), lalu
// mengonfirmasi sendiri bila antarmuka mendapat alamat dari jaringan itu;
// bila tidak, pengaturan lama dikembalikan.

// Agent memanggil operasi sapod (agent.Client di NAS).
type Agent interface {
	Call(ctx context.Context, op string, params any, out any) error
}

func (m *Menu) connectWiFi(ctx context.Context) bool {
	if m.Agent == nil {
		return m.fail(errors.New("sapod tidak tersedia"))
	}
	var st proto.NetworkStatus
	if err := m.Agent.Call(ctx, proto.OpNetworkStatus, nil, &st); err != nil {
		return m.fail(err)
	}
	var wifi []proto.NetworkLink
	for _, l := range st.Interfaces {
		if l.Wireless {
			wifi = append(wifi, l)
		}
	}
	if len(wifi) == 0 {
		m.printf("\n%s\n", m.t("wifiNone"))
		return true
	}
	if st.Pending != nil {
		m.printf("\n%s\n", m.t("wifiPending"))
		return true
	}
	iface := wifi[0].Name
	if len(wifi) > 1 {
		m.printf("\n")
		for i, l := range wifi {
			m.printf("  %d. %s\n", i+1, l.Name)
		}
		m.printf("%s", m.t("wifiPickInterface"))
		n, ok := m.readNumber(len(wifi))
		if !ok {
			m.printf("%s\n", m.t("cancelled"))
			return true
		}
		iface = wifi[n-1].Name
	}

	m.printf("\n%s\n", m.t("wifiScanning"))
	var seen []proto.WiFiSeen
	if err := m.Agent.Call(ctx, proto.OpNetworkWiFiScan, proto.WiFiScanParams{Interface: iface}, &seen); err != nil {
		return m.fail(err)
	}
	var usable []proto.WiFiSeen
	for _, s := range seen {
		if s.Secure {
			usable = append(usable, s)
		}
	}
	m.printf("\n")
	for i, s := range usable {
		m.printf("  %d. %s  (%s)\n", i+1, printable(s.SSID), m.signalText(s.SignalDBm))
	}
	m.printf("  0. %s\n%s", m.t("wifiHidden"), m.t("wifiPickNetwork"))
	choice, err := m.readLine()
	if err != nil || choice == "" {
		m.printf("%s\n", m.t("cancelled"))
		return true
	}
	var ssid string
	if choice == "0" {
		m.printf("%s", m.t("wifiTypeSSID"))
		if ssid, err = m.readRaw(); err != nil || ssid == "" {
			m.printf("%s\n", m.t("cancelled"))
			return true
		}
	} else {
		n, err := strconv.Atoi(choice)
		if err != nil || n < 1 || n > len(usable) {
			m.printf("%s\n", m.t("invalid"))
			return true
		}
		ssid = usable[n-1].SSID
	}
	m.printf("%s", m.t("wifiPassword", printable(ssid)))
	m.echo(false)
	password, err := m.readRaw()
	m.echo(true)
	m.printf("\n")
	if err != nil || password == "" {
		m.printf("%s\n", m.t("cancelled"))
		return true
	}

	cfg := wifiConfig(st, iface, ssid, password)
	var res proto.NetworkApplyResult
	if err := m.Agent.Call(ctx, proto.OpNetworkApply, proto.NetworkApplyParams{Config: cfg}, &res); err != nil {
		m.audit("recovery.wifi", false, map[string]string{"interface": iface, "ssid": ssid})
		return m.fail(err)
	}
	m.printf("%s\n", m.t("wifiConnecting", printable(ssid)))
	addr := m.waitWiFi(ctx, iface, ssid)
	tok := proto.NetworkTokenParams{Token: res.Token}
	if addr == "" {
		// Tidak tersambung (password salah, sinyal lemah): kembali ke
		// pengaturan lama sekarang juga, tanpa menunggu batas waktu.
		_ = m.Agent.Call(context.WithoutCancel(ctx), proto.OpNetworkCancel, tok, nil)
		m.audit("recovery.wifi", false, map[string]string{"interface": iface, "ssid": ssid})
		m.printf("\n%s\n", m.t("wifiFailed", printable(ssid)))
		return true
	}
	var cerr error
	for range 30 {
		if cerr = m.Agent.Call(ctx, proto.OpNetworkConfirm, tok, nil); cerr == nil {
			break
		}
		m.sleep(ctx, 2*time.Second)
	}
	m.audit("recovery.wifi", cerr == nil, map[string]string{"interface": iface, "ssid": ssid})
	if cerr != nil {
		return m.fail(cerr)
	}
	m.printf("\n%s\n", m.t("wifiConnected", printable(ssid), addr))
	return true
}

// wifiConfig: pengaturan jaringan sekarang (atau bawaan: kabel DHCP, Wi-Fi
// lain mati) dengan iface tersambung ke ssid lewat DHCP, kecuali sudah
// beralamat tetap.
func wifiConfig(st proto.NetworkStatus, iface, ssid, password string) proto.NetworkConfig {
	cfg := proto.NetworkConfig{Hostname: st.Hostname, DNS: []string{}}
	if st.Config != nil {
		cfg = *st.Config
		cfg.Interfaces = append([]proto.NetworkInterface(nil), st.Config.Interfaces...)
	} else {
		for _, l := range st.Interfaces {
			mode := "dhcp"
			if l.Wireless {
				mode = "off"
			}
			cfg.Interfaces = append(cfg.Interfaces, proto.NetworkInterface{Name: l.Name, Mode: mode})
		}
	}
	found := false
	for i := range cfg.Interfaces {
		nic := &cfg.Interfaces[i]
		if nic.Name != iface {
			continue
		}
		found = true
		if nic.Mode != "static" {
			nic.Mode, nic.Address, nic.Gateway = "dhcp", "", ""
		}
		nic.WiFi = &proto.WiFiNetwork{SSID: ssid, Password: password}
	}
	if !found {
		cfg.Interfaces = append(cfg.Interfaces, proto.NetworkInterface{Name: iface, Mode: "dhcp", WiFi: &proto.WiFiNetwork{SSID: ssid, Password: password}})
	}
	// Ada antarmuka DHCP: DNS dari router (sapod menolak DNS tetap).
	for _, nic := range cfg.Interfaces {
		if nic.Mode == "dhcp" {
			cfg.DNS = []string{}
		}
	}
	return cfg
}

// waitWiFi menunggu iface tersambung ke ssid dan mendapat alamat IPv4
// (paling lama 90 detik). Mengembalikan alamat itu, atau "".
func (m *Menu) waitWiFi(ctx context.Context, iface, ssid string) string {
	deadline := m.Now().Add(90 * time.Second)
	for m.Now().Before(deadline) && ctx.Err() == nil {
		m.sleep(ctx, 3*time.Second)
		var st proto.NetworkStatus
		if err := m.Agent.Call(ctx, proto.OpNetworkStatus, nil, &st); err != nil {
			continue
		}
		for _, l := range st.Interfaces {
			if l.Name != iface || l.SSID != ssid {
				continue
			}
			for _, a := range l.Addresses {
				if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Is4() {
					return p.Addr().String()
				}
			}
		}
	}
	return ""
}

func (m *Menu) signalText(dbm int) string {
	switch {
	case dbm >= -60:
		return m.t("wifiStrong")
	case dbm >= -72:
		return m.t("wifiMedium")
	}
	return m.t("wifiWeak")
}

// readNumber membaca nomor 1..max.
func (m *Menu) readNumber(max int) (int, bool) {
	s, err := m.readLine()
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil && n >= 1 && n <= max
}

func (m *Menu) echo(on bool) {
	if m.Echo != nil {
		m.Echo(on)
	}
}

func (m *Menu) sleep(ctx context.Context, d time.Duration) {
	if m.Sleep != nil {
		m.Sleep(d)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
