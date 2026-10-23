#!/usr/bin/env bash
# Uji asap ISO live: boot di QEMU, lalu tunggu sampai panel menjawab bahwa
# sapo-api dan sapod sehat dan wizard setup awal siap. Tidak ada disk yang
# dipasang ke VM, jadi uji ini tidak menulis ke disk mana pun.
# HTTPS wajib (ADR 0004): HTTP hanya boleh menyajikan halaman petunjuk, dan
# sidik jari yang diumumkan panel harus sama dengan sertifikat yang dipakai.
# Pemakaian: packaging/iso/smoke-test.sh dist/sapo-dev_<versi>_amd64.iso
# Kebutuhan: qemu-system-x86_64, curl, openssl. KVM dipakai bila tersedia.
set -euo pipefail

ISO="${1:?Pemakaian: $0 <file.iso>}"
PORT="${PORT:-15000}"              # port HTTP di host, diteruskan ke 5000 di VM
TLS_PORT="${TLS_PORT:-15001}"      # port HTTPS di host, diteruskan ke 5001 di VM
TIMEOUT_S="${TIMEOUT_S:-600}"      # batas waktu boot sampai panel siap
VM_RAM_MB="${VM_RAM_MB:-2048}"
LOG="${LOG:-$(dirname "$ISO")/smoke-test-console.log}"
HTTP="http://127.0.0.1:$PORT"
HTTPS="https://127.0.0.1:$TLS_PORT"

accel=(-accel tcg)
if [[ -w /dev/kvm ]]; then accel=(-accel kvm -cpu host); fi

qemu-system-x86_64 "${accel[@]}" -m "$VM_RAM_MB" -smp 2 \
  -cdrom "$ISO" -boot d \
  -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$PORT-:5000,hostfwd=tcp:127.0.0.1:$TLS_PORT-:5001" \
  -display none -serial "file:$LOG" &
qemu=$!
trap 'kill "$qemu" 2>/dev/null || true' EXIT

fail() { echo "GAGAL: $*" >&2; exit 1; }

echo "Menunggu panel di $HTTPS (maks ${TIMEOUT_S} detik)…"
deadline=$((SECONDS + TIMEOUT_S))
# -k: sertifikat self-signed; kecocokannya diperiksa lewat sidik jari di bawah.
until curl -kfsS --max-time 5 "$HTTPS/api/v1/health" 2>/dev/null | grep -q '"agent":true'; do
  kill -0 "$qemu" 2>/dev/null || fail "QEMU berhenti sebelum panel siap."
  (( SECONDS <= deadline )) || fail "Panel tidak siap dalam ${TIMEOUT_S} detik."
  sleep 5
done
echo "health (HTTPS): ok, sapo-api dan sapod berjalan setelah ${SECONDS} detik"

status="$(curl -kfsS --max-time 5 "$HTTPS/api/v1/auth/status")"
echo "auth/status (HTTPS): $status"
grep -q '"setup_required":true' <<<"$status" || fail "wizard setup awal seharusnya aktif di ISO baru."
curl -kfsS --max-time 5 "$HTTPS/" | grep -qi '<div id="app"' || fail "halaman panel tidak tersaji lewat HTTPS."

# Lewat HTTP: panel (halaman petunjuk) dan ui/config ada, API lain ditolak.
curl -fsS --max-time 5 "$HTTP/" | grep -qi '<div id="app"' || fail "halaman petunjuk tidak tersaji lewat HTTP."
# Termasuk permintaan yang membawa password (login, setup): ditolak sebelum
# diproses, dengan kode https_required.
for req in "GET /api/v1/auth/status" "POST /api/v1/auth/login" "POST /api/v1/setup/totp" "POST /api/v1/setup"; do
  read -r method path <<<"$req"
  body="$(curl -sS -X "$method" -H 'X-Sapo-Request: 1' -H 'Content-Type: application/json' \
    -d '{"username":"uji","password":"uji-password-panjang"}' -w ' %{http_code}' --max-time 5 "$HTTP$path")"
  [[ "$body" == *'"https_required"'*' 403' ]] || fail "$method $path lewat HTTP harus 403 https_required, dapat: $body"
done
echo "HTTP: hanya halaman petunjuk; API, login, dan setup ditolak (403 https_required)"

announced="$(curl -fsS --max-time 5 "$HTTP/api/v1/ui/config" | sed -n 's/.*"fingerprint_sha256":"\([0-9A-F:]*\)".*/\1/p')"
served="$(openssl s_client -connect "127.0.0.1:$TLS_PORT" -servername localhost </dev/null 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256 | cut -d= -f2)"
[[ -n "$announced" && "$announced" == "$served" ]] || fail "sidik jari diumumkan ($announced) beda dengan yang dipakai server ($served)."
echo "sidik jari sertifikat cocok: $served"

# Layar konsol (ditulis sapod) memuat sidik jari yang sama, dalam dua baris.
# Ditunggu sebentar: sapod menulisnya lalu meminta agetty menampilkan ulang.
for _ in $(seq 1 12); do
  grep -qF "${served:0:47}" "$LOG" && grep -qF "${served:48}" "$LOG" && break
  sleep 5
done
grep -qF "${served:0:47}" "$LOG" && grep -qF "${served:48}" "$LOG" \
  || fail "layar konsol tidak menampilkan sidik jari sertifikat yang dipakai server."
echo "layar konsol: sidik jari sama dengan sertifikat server"
echo "Uji asap lulus."
