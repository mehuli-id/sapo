#!/usr/bin/env bash
# Uji Wi-Fi sungguhan (ADR 0023) dengan radio virtual kernel
# (mac80211_hwsim): satu radio menjadi titik akses hostapd (WPA2, lalu
# WPA3-SAE saja), radio lain memakai file wpa_supplicant buatan sapod. SSID
# dan password sengaja berisi spasi, tanda kutip, dan #. Juga memeriksa
# parser keluaran iw sapod dengan keluaran iw sungguhan, dan bahwa password
# salah tidak tersambung.
# Pemakaian (Linux dengan sudo, misalnya runner CI): tools/wifi-hwsim/run.sh
set -euo pipefail
cd "$(dirname "$0")/../.."

SSID='Sapo Uji "WiFi" #1'
PASS='kata sandi: "rahasia" #1'
W="$(mktemp -d)"
cleanup() {
  for p in "$W"/*.pid; do [[ -f "$p" ]] && sudo kill "$(cat "$p")" 2>/dev/null || true; done
  sudo modprobe -r mac80211_hwsim 2>/dev/null || true
  rm -rf "$W"
}
trap cleanup EXIT

sudo modprobe mac80211_hwsim radios=2
sleep 2
mapfile -t radios < <(for i in /sys/class/net/*; do [[ -d "$i/phy80211" ]] && basename "$i"; done)
[[ ${#radios[@]} -ge 2 ]] || { echo "radio virtual tidak muncul: ${radios[*]}" >&2; exit 1; }
AP="${radios[0]}" STA="${radios[1]}"
ssid_hex="$(printf '%s' "$SSID" | od -An -tx1 | tr -d ' \n')"

stop() { # stop NAMA
  [[ -f "$W/$1.pid" ]] && sudo kill "$(cat "$W/$1.pid")" 2>/dev/null || true
  rm -f "$W/$1.pid"
  sleep 1
}

# connect KUNCI PASSWORD_STA: titik akses dengan kunci (WPA-PSK atau SAE),
# stasiun memakai file sapod dengan PASSWORD_STA. Hasil: 0 bila tersambung.
connect() {
  local key="$1" sta_pass="$2"
  {
    printf 'interface=%s\ndriver=nl80211\nssid2=%s\nhw_mode=g\nchannel=6\ncountry_code=ID\n' "$AP" "$ssid_hex"
    printf 'wpa=2\nrsn_pairwise=CCMP\nwpa_key_mgmt=%s\n' "$key"
    if [[ "$key" == SAE ]]; then
      printf 'ieee80211w=2\nsae_require_mfp=1\nsae_password=%s\n' "$PASS"
    else
      printf 'wpa_passphrase=%s\n' "$PASS"
    fi
  } > "$W/hostapd.conf"
  sudo hostapd -B -P "$W/hostapd.pid" "$W/hostapd.conf" >/dev/null
  SAPO_WPA_CONF_OUT="$W/sapo.conf" SAPO_WIFI_SSID="$SSID" SAPO_WIFI_PASSWORD="$sta_pass" \
    go test ./internal/agent -run '^TestHwsimWPAConf$' -count=1 >/dev/null
  sudo ip link set "$STA" up
  for _ in $(seq 1 10); do
    sudo iw dev "$STA" scan > "$W/scan.txt" 2>/dev/null && grep -q "SSID: " "$W/scan.txt" && break
    sleep 1
  done
  sudo wpa_supplicant -B -i "$STA" -c "$W/sapo.conf" -P "$W/wpa.pid" >/dev/null
  local ok=1
  for _ in $(seq 1 20); do
    if sudo iw dev "$STA" link > "$W/link.txt" && grep -q "SSID: " "$W/link.txt"; then ok=0; break; fi
    sleep 1
  done
  if [[ $ok == 0 ]]; then
    # Parser iw sapod dengan keluaran iw sungguhan.
    SAPO_IW_SCAN="$W/scan.txt" SAPO_IW_LINK="$W/link.txt" SAPO_WIFI_SSID="$SSID" \
      go test ./internal/agent -run '^TestHwsimIw$' -count=1 || ok=2
  fi
  stop wpa
  stop hostapd
  return $ok
}

rc=0; connect WPA-PSK "$PASS" || rc=$?
[[ $rc == 0 ]] || { echo "WPA2: file sapod tidak tersambung atau parser iw salah ($rc)" >&2; exit 1; }
echo "WPA2: tersambung dengan file wpa_supplicant buatan sapod"
rc=0; connect SAE "$PASS" || rc=$?
[[ $rc == 0 ]] || { echo "WPA3 (SAE): file sapod tidak tersambung atau parser iw salah ($rc)" >&2; exit 1; }
echo "WPA3 (SAE saja): tersambung dengan file wpa_supplicant buatan sapod"
rc=0; connect WPA-PSK "password-yang-salah" || rc=$?
[[ $rc == 1 ]] || { echo "password salah tidak boleh tersambung ($rc)" >&2; exit 1; }
echo "Password salah: tidak tersambung"
