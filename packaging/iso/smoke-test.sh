#!/usr/bin/env bash
# Uji asap ISO live: boot di QEMU, lalu tunggu sampai panel menjawab bahwa
# sapo-api dan sapod sehat dan wizard setup awal siap. Tidak ada disk yang
# dipasang ke VM, jadi uji ini tidak menulis ke disk mana pun.
# Pemakaian: packaging/iso/smoke-test.sh dist/sapo-dev_<versi>_amd64.iso
# Kebutuhan: qemu-system-x86_64, curl. KVM dipakai bila tersedia.
set -euo pipefail

ISO="${1:?Pemakaian: $0 <file.iso>}"
PORT="${PORT:-15000}"              # port di host, diteruskan ke 5000 di VM
TIMEOUT_S="${TIMEOUT_S:-600}"      # batas waktu boot sampai panel siap
VM_RAM_MB="${VM_RAM_MB:-2048}"
LOG="${LOG:-$(dirname "$ISO")/smoke-test-console.log}"
BASE="http://127.0.0.1:$PORT"

accel=(-accel tcg)
if [[ -w /dev/kvm ]]; then accel=(-accel kvm -cpu host); fi

qemu-system-x86_64 "${accel[@]}" -m "$VM_RAM_MB" -smp 2 \
  -cdrom "$ISO" -boot d \
  -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$PORT-:5000" \
  -display none -serial "file:$LOG" &
qemu=$!
trap 'kill "$qemu" 2>/dev/null || true' EXIT

echo "Menunggu panel di $BASE (maks ${TIMEOUT_S} detik)…"
deadline=$((SECONDS + TIMEOUT_S))
until curl -fsS --max-time 5 "$BASE/api/v1/health" 2>/dev/null | grep -q '"agent":true'; do
  if ! kill -0 "$qemu" 2>/dev/null; then echo "QEMU berhenti sebelum panel siap." >&2; exit 1; fi
  if (( SECONDS > deadline )); then echo "Panel tidak siap dalam ${TIMEOUT_S} detik." >&2; exit 1; fi
  sleep 5
done
echo "health: ok (sapo-api dan sapod berjalan) setelah ${SECONDS} detik"

status="$(curl -fsS --max-time 5 "$BASE/api/v1/auth/status")"
echo "auth/status: $status"
grep -q '"setup_required":true' <<<"$status" || { echo "Wizard setup awal seharusnya aktif di ISO baru." >&2; exit 1; }

curl -fsS --max-time 5 "$BASE/" | grep -qi '<div id="app"' || { echo "Halaman panel tidak tersaji." >&2; exit 1; }
echo "Panel tersaji. Uji asap lulus."
