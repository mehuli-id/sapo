#!/usr/bin/env bash
# Uji asap ISO live: boot di QEMU, lalu tunggu sampai panel menjawab bahwa
# sapo-api dan sapod sehat dan wizard setup awal siap. Setelah itu setup awal
# dijalankan lewat API dan pool ZFS mirror dibuat di dua disk virtual
# sementara (file di samping ISO, dihapus di akhir); disk ketiga berisi
# ext4 dan harus ditolak tanpa konfirmasi penghapusan (ADR 0005). Setelah
# scrub, shared folder dibuat dan dibuka lewat SMB dengan tiga pengguna
# (tulis, baca, tanpa akses), satu disk diganti disk ketiga (resilver), lalu
# pool dihapus.
# HTTPS wajib (ADR 0004): HTTP hanya boleh menyajikan halaman petunjuk, dan
# sidik jari yang diumumkan panel harus sama dengan sertifikat yang dipakai.
# Pemakaian: packaging/iso/smoke-test.sh dist/sapo-dev_<versi>_amd64.iso
# Kebutuhan: qemu-system-x86_64, curl, openssl, python3, mkfs.ext4, smbclient. KVM
# dipakai bila tersedia.
set -euo pipefail

ISO="${1:?Pemakaian: $0 <file.iso>}"
PORT="${PORT:-15000}"              # port HTTP di host, diteruskan ke 5000 di VM
TLS_PORT="${TLS_PORT:-15001}"      # port HTTPS di host, diteruskan ke 5001 di VM
SMB_PORT="${SMB_PORT:-14450}"      # port SMB di host, diteruskan ke 445 di VM
TIMEOUT_S="${TIMEOUT_S:-600}"      # batas waktu boot sampai panel siap
VM_RAM_MB="${VM_RAM_MB:-2048}"
LOG="${LOG:-$(dirname "$ISO")/smoke-test-console.log}"
HTTP="http://127.0.0.1:$PORT"
HTTPS="https://127.0.0.1:$TLS_PORT"

# Disk virtual untuk uji pool: dua kosong, satu berisi ext4 berlabel.
WORK="$(mktemp -d)"
for i in 1 2 3; do truncate -s 2G "$WORK/disk$i.img"; done
mkfs.ext4 -q -L foto "$WORK/disk3.img"
disks=()
for i in 1 2 3; do
  disks+=(-drive "file=$WORK/disk$i.img,if=none,format=raw,id=d$i" -device "virtio-blk-pci,drive=d$i,serial=SAPOUJI$i")
done

accel=(-accel tcg)
if [[ -w /dev/kvm ]]; then accel=(-accel kvm -cpu host); fi

qemu-system-x86_64 "${accel[@]}" -m "$VM_RAM_MB" -smp 2 \
  -cdrom "$ISO" -boot d "${disks[@]}" \
  -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$PORT-:5000,hostfwd=tcp:127.0.0.1:$TLS_PORT-:5001,hostfwd=tcp:127.0.0.1:$SMB_PORT-:445" \
  -display none -serial "file:$LOG" &
qemu=$!
trap 'kill "$qemu" 2>/dev/null || true; rm -rf "$WORK"' EXIT

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

# --- Setup awal dan pool ZFS lewat API --------------------------------------
JAR="$WORK/cookies"
api() { # api METHOD PATH [JSON] → badan jawaban + " <kode HTTP>"
  local body=()
  if [[ $# -ge 3 ]]; then body=(-d "$3"); fi
  curl -ksS -b "$JAR" -c "$JAR" -X "$1" -H 'X-Sapo-Request: 1' -H 'Content-Type: application/json' \
    "${body[@]}" -w ' %{http_code}' --max-time 150 "$HTTPS$2"
}
json() { python3 -c 'import json,sys; v=json.loads(sys.stdin.read().rsplit(" ",1)[0]); exec(sys.argv[1])' "$1"; }
totp() { python3 - "$1" <<'PY'
import base64, hashlib, hmac, struct, sys, time
s = sys.argv[1]; key = base64.b32decode(s + "=" * (-len(s) % 8))
h = hmac.new(key, struct.pack(">Q", int(time.time()) // 30), hashlib.sha1).digest()
o = h[-1] & 15
print("%06d" % ((struct.unpack(">I", h[o:o + 4])[0] & 0x7FFFFFFF) % 1000000))
PY
}
PASS="uji-asap-$(openssl rand -hex 8)"
enroll="$(api POST /api/v1/setup/totp '{"username":"penguji"}')"
[[ "$enroll" == *' 200' ]] || fail "setup/totp: $enroll"
secret="$(json 'print(v["secret"])' <<<"$enroll")"
token="$(json 'print(v["token"])' <<<"$enroll")"
res="$(api POST /api/v1/setup "{\"language\":\"id\",\"username\":\"penguji\",\"password\":\"$PASS\",\"password_confirm\":\"$PASS\",\"totp_code\":\"$(totp "$secret")\",\"totp_token\":\"$token\"}")"
[[ "$res" == *' 200' ]] || fail "setup: $res"
echo "setup awal lewat API: ok"

pools="$(api GET /api/v1/storage/pools)"
[[ "$pools" == *' 200' ]] || fail "storage/pools: $pools"
json 'assert v["available"], v.get("message"); assert v["pools"] == [], v' <<<"$pools" || fail "ZFS harus siap tanpa pool: $pools"
disks_json="$(api GET /api/v1/storage/disks)"
# Identitas disk uji (nomor seri SAPOUJIn) seperti yang dilihat panel.
refs() { json "
d={x['serial']:x for x in v}
out=[]
for s,erase in [$1]:
    x=d[s]; out.append(dict(name=x['name'],serial=x['serial'],wwn=x['wwn'],size_bytes=x['size_bytes'],usage=x['usage'],erase=erase))
print(json.dumps(out))" <<<"$disks_json"; }
json "
u={x['serial']:x['usage'] for x in v}
assert u.get('SAPOUJI1')=='free' and u.get('SAPOUJI2')=='free' and u.get('SAPOUJI3')=='data', u" <<<"$disks_json" \
  || fail "status disk uji salah: $disks_json"

# Disk berisi ext4 tanpa konfirmasi penghapusan: ditolak, tidak ada yang dihapus.
res="$(api POST /api/v1/storage/pools "{\"name\":\"uji\",\"layout\":\"mirror\",\"disks\":$(refs "('SAPOUJI1',True),('SAPOUJI3',False)"),\"confirm_name\":\"uji\",\"password\":\"$PASS\"}")"
[[ "$res" == *'"disk_has_data"'*' 409' ]] || fail "disk berisi data harus ditolak: $res"
# Password salah: ditolak sebelum sapod dipanggil.
res="$(api POST /api/v1/storage/pools "{\"name\":\"uji\",\"layout\":\"mirror\",\"disks\":$(refs "('SAPOUJI1',True),('SAPOUJI2',True)"),\"confirm_name\":\"uji\",\"password\":\"salah-sekali-ini\"}")"
[[ "$res" == *' 401' ]] || fail "password salah harus 401: $res"
echo "penolakan: disk berisi data (409) dan password salah (401)"

res="$(api POST /api/v1/storage/pools "{\"name\":\"uji\",\"layout\":\"mirror\",\"disks\":$(refs "('SAPOUJI1',True),('SAPOUJI2',True)"),\"confirm_name\":\"uji\",\"password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "membuat pool mirror: $res"
json 'assert v["name"]=="uji" and v["health"]=="ONLINE" and v["layout"]=="mirror", v
leaves=[c for t in v["vdevs"] for c in t.get("children",[])]
assert len(leaves)==2 and all(c["state"]=="ONLINE" and c["disk"] for c in leaves), leaves' <<<"$res" \
  || fail "pool baru tidak sesuai: $res"
echo "pool mirror dibuat: $(json 'print(v["name"], v["health"], v["size_bytes"], [c["disk"] for t in v["vdevs"] for c in t["children"]])' <<<"$res")"

# Disk anggota pool sekarang tidak bisa dipilih lagi.
disks_json="$(api GET /api/v1/storage/disks)"
json "assert all(x['usage']=='in_use' and x['usage_reason']=='pool' for x in v if x['serial'] in ('SAPOUJI1','SAPOUJI2')), v" <<<"$disks_json" \
  || fail "disk pool harus berstatus in_use/pool: $disks_json"
res="$(api POST /api/v1/storage/pools "{\"name\":\"uji2\",\"layout\":\"single\",\"disks\":$(refs "('SAPOUJI1',True)"),\"confirm_name\":\"uji2\",\"password\":\"$PASS\"}")"
[[ "$res" == *'"disk_in_use"'*' 409' ]] || fail "disk anggota pool harus ditolak: $res"

res="$(api POST /api/v1/storage/pools/uji/scrub '{"action":"start"}')"
[[ "$res" == *' 200' ]] || fail "scrub: $res"
for _ in $(seq 1 30); do
  pools="$(api GET /api/v1/storage/pools)"
  json 'import sys; s=v["pools"][0].get("scan") or {}; sys.exit(0 if s.get("function")=="scrub" and s.get("state")=="finished" else 1)' <<<"$pools" && break
  sleep 2
done
json 's=v["pools"][0]["scan"]; assert s["function"]=="scrub" and s["state"]=="finished" and s["errors"]==0, s' <<<"$pools" \
  || fail "scrub tidak selesai bersih: $pools"
echo "scrub selesai tanpa kesalahan"

# --- Shared folder lewat SMB ---------------------------------------------------
UPASS="uji-smb-$(openssl rand -hex 6)"
for u in budi ani cici; do
  res="$(api POST /api/v1/users "{\"username\":\"$u\",\"password\":\"$UPASS\",\"password_confirm\":\"$UPASS\",\"role\":\"user\",\"groups\":[],\"must_change_password\":false,\"admin_password\":\"$PASS\"}")"
  [[ "$res" == *' 200' && "$res" != *'"warning":"system_sync_failed"'* ]] || fail "buat pengguna $u: $res"
done
res="$(api POST /api/v1/shares "{\"name\":\"keluarga\",\"pool\":\"uji\",\"description\":\"Uji asap\",\"permissions\":{\"user:budi\":\"write\",\"user:ani\":\"read\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "buat shared folder: $res"
json 'assert not v.get("error") and v["path"].endswith("/uji/keluarga"), v' <<<"$res" || fail "shared folder tidak siap: $res"
smb() { # smb USER PERINTAH → keluaran smbclient (tidak gagal walau smbclient gagal)
  smbclient "//127.0.0.1/keluarga" -p "$SMB_PORT" -m SMB3 -U "$1%$UPASS" -c "$2" 2>&1 || true
}
echo "isi uji asap" > "$WORK/halo.txt"
out="$(smb budi "put $WORK/halo.txt halo.txt; ls")"
[[ "$out" == *"halo.txt"* ]] || fail "budi (tulis) harus bisa menulis: $out"
out="$(smb ani "get halo.txt $WORK/salinan.txt")"
cmp -s "$WORK/halo.txt" "$WORK/salinan.txt" || fail "ani (baca) harus bisa membaca: $out"
out="$(smb ani "put $WORK/halo.txt coba.txt")"
[[ "$out" == *"NT_STATUS_ACCESS_DENIED"* ]] || fail "ani (baca) tidak boleh menulis: $out"
out="$(smb cici "ls")"
[[ "$out" == *"NT_STATUS_ACCESS_DENIED"* ]] || fail "cici (tanpa izin) harus ditolak: $out"
out="$(smbclient //127.0.0.1/keluarga -p "$SMB_PORT" -m SMB3 -U "budi%salah-sekali" -c ls 2>&1 || true)"
[[ "$out" == *"NT_STATUS_LOGON_FAILURE"* ]] || fail "password salah harus ditolak: $out"
out="$(smbclient //127.0.0.1/keluarga -p "$SMB_PORT" -m SMB3 -N -c ls 2>&1 || true)"
[[ "$out" != *"halo.txt"* ]] || fail "tamu (tanpa login) tidak boleh masuk: $out"
echo "SMB: tulis (budi), baca saja (ani), tanpa izin (cici), password salah, dan tamu sesuai izin"

# Izin baru berlaku juga untuk isi folder yang sudah ada (diterapkan sapod
# di latar belakang): cici diberi izin baca setelah halo.txt ditulis.
res="$(api POST /api/v1/shares/keluarga "{\"permissions\":{\"user:budi\":\"write\",\"user:ani\":\"read\",\"user:cici\":\"read\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' && "$res" != *'system_sync_failed'* ]] || fail "ubah izin: $res"
for _ in $(seq 1 30); do
  shares="$(api GET /api/v1/shares)"
  json 'import sys; s=v[0]; sys.exit(1 if s.get("acl_pending") else 0)' <<<"$shares" && break
  sleep 2
done
json 's=v[0]; assert not s.get("acl_pending") and not s.get("acl_error"), s' <<<"$shares" || fail "izin isi folder: $shares"
rm -f "$WORK/salinan.txt"
out="$(smb cici "get halo.txt $WORK/salinan.txt")"
cmp -s "$WORK/halo.txt" "$WORK/salinan.txt" || fail "cici (baca, izin baru) harus bisa membaca file lama: $out"
echo "SMB: izin baru berlaku untuk file yang sudah ada"

# Pengguna yang dinonaktifkan tidak bisa masuk lewat SMB, dan sesinya yang
# sedang terbuka ikut diputus.
smbclient //127.0.0.1/keluarga -p "$SMB_PORT" -m SMB3 -U "ani%$UPASS" -c "ls; !sleep 15; ls" > "$WORK/sesi.txt" 2>&1 &
sesi=$!
sleep 5
res="$(api POST /api/v1/users/ani "{\"disabled\":true}")"
[[ "$res" == *' 200' ]] || fail "nonaktifkan ani: $res"
wait "$sesi" || true
[[ "$(grep -c 'halo.txt' "$WORK/sesi.txt")" == 1 ]] || fail "sesi ani yang terbuka harus diputus: $(cat "$WORK/sesi.txt")"
out="$(smb ani "ls")"
[[ "$out" == *"NT_STATUS"* && "$out" != *"halo.txt"* ]] || fail "ani nonaktif harus ditolak: $out"
echo "SMB: pengguna nonaktif ditolak dan sesinya diputus"

# Hapus folder: nama salah ditolak, lalu folder dan isinya hilang.
res="$(api POST /api/v1/shares/keluarga/delete "{\"confirm_name\":\"kel\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *'"confirm_mismatch"'*' 400' ]] || fail "nama salah harus ditolak: $res"
res="$(api POST /api/v1/shares/keluarga/delete "{\"confirm_name\":\"keluarga\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus shared folder: $res"
out="$(smb budi "ls")"
[[ "$out" == *"NT_STATUS_BAD_NETWORK_NAME"* ]] || fail "folder terhapus masih bisa dibuka: $out"
echo "shared folder dihapus"

# Ganti disk SAPOUJI2 dengan SAPOUJI3 (berisi ext4, penghapusan dikonfirmasi).
disks_json="$(api GET /api/v1/storage/disks)"
old_disk="$(json "print([x['name'] for x in v if x['serial']=='SAPOUJI2'][0])" <<<"$disks_json")"
pools="$(api GET /api/v1/storage/pools)"
old_guid="$(json "print([c['guid'] for t in v['pools'][0]['vdevs'] for c in t['children'] if c['disk']=='$old_disk'][0])" <<<"$pools")"
new_ref="$(refs "('SAPOUJI3',True)" | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)[0]))')"
res="$(api POST /api/v1/storage/pools/uji/replace "{\"old_guid\":\"$old_guid\",\"disk\":$new_ref,\"password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "ganti disk: $res"
for _ in $(seq 1 60); do
  pools="$(api GET /api/v1/storage/pools)"
  json 'import sys
p=v["pools"][0]; s=p.get("scan") or {}
kids=[c for t in p["vdevs"] for c in t.get("children",[])]
# Disk lama baru dilepas (vdev replacing hilang) sesaat setelah resilver selesai.
done=s.get("function")=="resilver" and s.get("state")=="finished" and p["health"]=="ONLINE"
sys.exit(0 if done and len(kids)==2 and all(c["type"]=="disk" for c in kids) else 1)' <<<"$pools" && break
  sleep 2
done
disks_json="$(api GET /api/v1/storage/disks)"
new_disk="$(json "print([x['name'] for x in v if x['serial']=='SAPOUJI3'][0])" <<<"$disks_json")"
json "p=v['pools'][0]; leaves=sorted(c.get('disk','') for t in p['vdevs'] for c in t['children'])
assert p['health']=='ONLINE' and p['scan']['function']=='resilver' and p['scan']['state']=='finished', p
assert '$new_disk' in leaves and '$old_disk' not in leaves, leaves" <<<"$pools" || fail "resilver tidak selesai dengan benar: $pools"
# Disk baru anggota pool. Disk lama yang dilepas masih berlabel nama pool
# yang aktif, jadi sengaja tetap dianggap dipakai (gagal-tertutup, ADR 0005).
json "u={x['serial']:(x['usage'],x.get('usage_reason','')) for x in v}
assert u['SAPOUJI2']==('in_use','pool') and u['SAPOUJI3']==('in_use','pool'), u" <<<"$disks_json" \
  || fail "status disk setelah ganti salah: $disks_json"
echo "disk $old_disk diganti $new_disk, resilver selesai"

# Hapus pool: nama salah ditolak, lalu dihapus sungguhan.
res="$(api POST /api/v1/storage/pools/uji/destroy "{\"confirm_name\":\"ujj\",\"password\":\"$PASS\"}")"
[[ "$res" == *'"confirm_mismatch"'*' 400' ]] || fail "nama salah harus ditolak: $res"
res="$(api POST /api/v1/storage/pools/uji/destroy "{\"confirm_name\":\"uji\",\"password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus pool: $res"
pools="$(api GET /api/v1/storage/pools)"
json 'assert v["pools"]==[], v' <<<"$pools" || fail "pool masih ada: $pools"
# Label pool terhapus tersisa: disk tampil berisi data, boleh dipakai lagi.
disks_json="$(api GET /api/v1/storage/disks)"
json "u={x['serial']:(x['usage'],x.get('usage_reason','')) for x in v}
assert all(u[s]==('data','zfs_label') for s in ('SAPOUJI1','SAPOUJI2','SAPOUJI3')), u" <<<"$disks_json" \
  || fail "disk pool terhapus harus berstatus data/zfs_label: $disks_json"
echo "pool dihapus; disknya tampil berisi data (label ZFS lama)"
echo "Uji asap lulus."
