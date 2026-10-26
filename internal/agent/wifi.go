package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Wi-Fi (ADR 0023). Antarmuka nirkabel diatur seperti antarmuka kabel
// (network.apply, konfirmasi, pengembalian otomatis), ditambah jaringan
// WPA2/WPA3 Personal. sapod menulis satu file wpa_supplicant per antarmuka
// (0600, root) yang dirujuk /etc/network/interfaces lewat "wpa-conf"; SSID dan
// password ditulis heksadesimal sehingga tidak ada teks dari panel yang bisa
// menambah baris. Pindai jaringan dan keadaan sambungan lewat iw.

// wifiCountryPattern: kode negara regulasi Wi-Fi (ISO 3166-1 alfa-2).
var wifiCountryPattern = regexp.MustCompile(`^[A-Z]{2}$`)

// wireless: antarmuka nirkabel (cfg80211).
func (n *Network) wireless(name string) bool {
	for _, f := range []string{"wireless", "phy80211"} {
		if _, err := os.Stat(filepath.Join(n.t.NetDir, name, f)); err == nil {
			return true
		}
	}
	return false
}

func (n *Network) wpaConfPath(name string) string {
	return filepath.Join(n.t.WPAConfDir, "sapo-"+name+".conf")
}

// checkWiFi memeriksa SSID (1–32 byte UTF-8 tanpa karakter kendali) dan
// password (8–63 karakter ASCII yang bisa dicetak, atau kosong = tetap).
func checkWiFi(w *proto.WiFiNetwork) error {
	if w == nil {
		return errors.New("pilih jaringan Wi-Fi")
	}
	if len(w.SSID) == 0 || len(w.SSID) > 32 || !utf8.ValidString(w.SSID) || strings.ContainsFunc(w.SSID, unicode.IsControl) {
		return errors.New("nama jaringan Wi-Fi (SSID) harus 1–32 byte tanpa karakter kendali")
	}
	if w.Password == "" {
		return nil
	}
	if len(w.Password) < 8 || len(w.Password) > 63 || strings.ContainsFunc(w.Password, func(r rune) bool { return r < 0x20 || r > 0x7e }) {
		return errors.New("password Wi-Fi harus 8–63 karakter (huruf, angka, dan tanda baca biasa)")
	}
	return nil
}

// renderWPAConf menyusun file wpa_supplicant untuk satu jaringan WPA2/WPA3
// Personal. SSID dan password heksadesimal (tanpa tanda kutip yang bisa
// disalahgunakan); PSK WPA2 dihitung di sini (PBKDF2), WPA3 (SAE) memakai
// password itu sendiri. scan_ssid=1: jaringan tersembunyi juga bisa.
func renderWPAConf(country string, w proto.WiFiNetwork) ([]byte, error) {
	if !wifiCountryPattern.MatchString(country) {
		return nil, errors.New("kode negara Wi-Fi tidak valid")
	}
	psk, err := pbkdf2.Key(sha1.New, w.Password, []byte(w.SSID), 4096, 32)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("# Dibuat oleh sapod (Sapo). Jangan diubah manual: atur Wi-Fi lewat panel atau menu konsol.\n")
	b.WriteString("ctrl_interface=DIR=/run/wpa_supplicant GROUP=root\nupdate_config=0\n")
	fmt.Fprintf(&b, "country=%s\n", country)
	b.WriteString("network={\n")
	fmt.Fprintf(&b, "\tssid=%s\n", hex.EncodeToString([]byte(w.SSID)))
	b.WriteString("\tscan_ssid=1\n\tkey_mgmt=WPA-PSK WPA-PSK-SHA256 SAE\n\tieee80211w=1\n")
	fmt.Fprintf(&b, "\tpsk=%s\n", hex.EncodeToString(psk))
	fmt.Fprintf(&b, "\tsae_password=%s\n", hex.EncodeToString([]byte(w.Password)))
	b.WriteString("}\n")
	return b.Bytes(), nil
}

// iwSafe: nama antarmuka yang boleh menjadi argumen iw (iw tidak menerima
// "--", jadi nama tidak boleh berawal "-").
func (n *Network) iwSafe(name string) bool {
	return ifacePattern.MatchString(name) && !strings.HasPrefix(name, "-") && n.wireless(name)
}

// unescapeIw: SSID dari iw (byte tak tercetak sebagai \xNN).
func unescapeIw(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseIwScan: hasil "iw dev <x> scan", sinyal terkuat per SSID, urut
// sinyal. Jaringan tersembunyi (SSID kosong) dilewati. Secure hanya untuk
// WPA/WPA2/WPA3 Personal (PSK/SAE); jaringan terbuka dan Enterprise tidak.
func parseIwScan(out []byte) []proto.WiFiSeen {
	type bss struct {
		ssid     string
		signal   float64
		inSec    bool
		personal bool
	}
	var all []bss
	var cur *bss
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "BSS "):
			all = append(all, bss{signal: -1000})
			cur = &all[len(all)-1]
			continue
		case cur == nil:
			continue
		}
		depth := len(line) - len(strings.TrimLeft(line, "\t"))
		if depth == 1 {
			cur.inSec = strings.HasPrefix(t, "RSN:") || strings.HasPrefix(t, "WPA:")
		}
		switch {
		case depth == 1 && strings.HasPrefix(t, "SSID:"):
			cur.ssid = unescapeIw(strings.TrimPrefix(strings.TrimPrefix(t, "SSID:"), " "))
		case depth == 1 && strings.HasPrefix(t, "signal:"):
			f := strings.Fields(strings.TrimPrefix(t, "signal:"))
			if len(f) > 0 {
				if v, err := strconv.ParseFloat(f[0], 64); err == nil {
					cur.signal = v
				}
			}
		case cur.inSec && strings.Contains(t, "Authentication suites:"):
			suites := strings.Fields(t[strings.Index(t, ":")+1:])
			for _, s := range suites {
				if s == "PSK" || s == "SAE" || s == "PSK/SHA-256" || s == "FT/PSK" || s == "FT/SAE" {
					cur.personal = true
				}
			}
		}
	}
	best := map[string]proto.WiFiSeen{}
	for _, b := range all {
		if b.ssid == "" || !utf8.ValidString(b.ssid) || len(b.ssid) > 32 || strings.ContainsFunc(b.ssid, unicode.IsControl) {
			continue
		}
		s := proto.WiFiSeen{SSID: b.ssid, SignalDBm: int(b.signal), Secure: b.personal}
		if old, ok := best[b.ssid]; !ok || s.SignalDBm > old.SignalDBm || (s.Secure && !old.Secure) {
			best[b.ssid] = s
		}
	}
	list := make([]proto.WiFiSeen, 0, len(best))
	for _, s := range best {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].SignalDBm != list[j].SignalDBm {
			return list[i].SignalDBm > list[j].SignalDBm
		}
		return list[i].SSID < list[j].SSID
	})
	if len(list) > 50 {
		list = list[:50]
	}
	return list
}

// parseIwLink: SSID dan sinyal dari "iw dev <x> link" (kosong bila tidak
// tersambung).
func parseIwLink(out []byte) (string, int) {
	ssid, signal := "", 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(t, "SSID:"):
			ssid = unescapeIw(strings.TrimPrefix(strings.TrimPrefix(t, "SSID:"), " "))
		case strings.HasPrefix(t, "signal:"):
			if f := strings.Fields(strings.TrimPrefix(t, "signal:")); len(f) > 0 {
				if v, err := strconv.ParseFloat(f[0], 64); err == nil {
					signal = int(v)
				}
			}
		}
	}
	if !utf8.ValidString(ssid) || strings.ContainsFunc(ssid, unicode.IsControl) {
		ssid = ""
	}
	return ssid, signal
}

// wifiLink: jaringan yang sedang tersambung di antarmuka nirkabel.
func (n *Network) wifiLink(ctx context.Context, name string) (string, int) {
	if n.t.Iw == "" || !n.iwSafe(name) {
		return "", 0
	}
	res, err := n.runArgs(ctx, n.t.Iw, "dev", name, "link")
	if err != nil || res.ExitCode != 0 {
		return "", 0
	}
	return parseIwLink(res.Stdout)
}

// WiFiScanOp: network.wifi_scan. Antarmuka dinyalakan dulu (tanpa alamat)
// bila belum; bila sedang sibuk memindai (wpa_supplicant), hasil pindai
// terakhir dipakai.
func (n *Network) WiFiScanOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.WiFiScanParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		phys, err := n.physicalInterfaces()
		if err != nil {
			return nil, err
		}
		if !strings.Contains(" "+strings.Join(phys, " ")+" ", " "+p.Interface+" ") || !n.iwSafe(p.Interface) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "bukan antarmuka Wi-Fi"}
		}
		if n.t.Iw == "" {
			return nil, &OpError{Code: proto.ErrUnavailable, Message: "program iw tidak terpasang"}
		}
		_, _ = n.run(ctx, n.t.IP, nil, "link", "set", "dev", p.Interface, "up")
		res, err := n.runArgs(ctx, n.t.Iw, "dev", p.Interface, "scan")
		if err == nil && res.ExitCode != 0 && strings.Contains(string(res.Stderr), "busy") {
			res, err = n.runArgs(ctx, n.t.Iw, "dev", p.Interface, "scan", "dump")
		}
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			msg := firstLine(res.Stderr)
			if strings.Contains(msg, "rfkill") || strings.Contains(msg, "-132") {
				msg = "Wi-Fi dimatikan saklar atau tombol pesawat di komputer"
			}
			return nil, &OpError{Code: "wifi_scan_failed", Message: clipTo(msg, maxErrorText)}
		}
		return parseIwScan(res.Stdout), nil
	}
}
