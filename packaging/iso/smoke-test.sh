#!/usr/bin/env bash
# Uji asap ISO live: boot di QEMU, lalu tunggu sampai panel menjawab bahwa
# sapo-api dan sapod sehat dan wizard setup awal siap. Setelah itu setup awal
# dijalankan lewat API dan pool ZFS mirror dibuat di dua disk virtual
# sementara (file di samping ISO, dihapus di akhir); disk ketiga berisi
# ext4 dan harus ditolak tanpa konfirmasi penghapusan (ADR 0005). Setelah
# scrub, shared folder dibuat dan dibuka lewat SMB dengan tiga pengguna
# (tulis, baca, tanpa akses) dan lewat NFS (dipasang dari host dengan sudo),
# satu disk diganti disk ketiga (resilver), lalu pool dihapus. Terakhir, menu
# darurat di port serial (ADR 0008) me-reset password admin dan jaringan.
# Sebelumnya pengaturan jaringan diubah dari panel (ADR 0009): satu
# dikonfirmasi, satu dikembalikan otomatis karena panel tidak terjangkau.
# HTTPS wajib (ADR 0004): HTTP hanya boleh menyajikan halaman petunjuk, dan
# sidik jari yang diumumkan panel harus sama dengan sertifikat yang dipakai.
# Pemakaian: packaging/iso/smoke-test.sh dist/sapo-dev_<versi>_amd64.iso
# Kebutuhan: qemu-system-x86_64, curl, openssl, python3, mkfs.ext4, mkfs.exfat, smbclient,
# klien NFS (mount.nfs4) dan sudo tanpa password. KVM dipakai bila tersedia.
set -euo pipefail

ISO="${1:?Pemakaian: $0 <file.iso>}"
PORT="${PORT:-15000}"              # port HTTP di host, diteruskan ke 5000 di VM
TLS_PORT="${TLS_PORT:-15001}"      # port HTTPS di host, diteruskan ke 5001 di VM
SMB_PORT="${SMB_PORT:-14450}"      # port SMB di host, diteruskan ke 445 di VM
NFS_PORT="${NFS_PORT:-12049}"      # port NFS di host, diteruskan ke 2049 di VM
APP_PORT="${APP_PORT:-18090}"      # port aplikasi uji (Homer) di host, diteruskan ke 8090 di VM
RPCBIND_PORT="${RPCBIND_PORT:-10111}" # diteruskan ke 111 (rpcbind) di VM, harus tertutup
MOUNTD_PORT="${MOUNTD_PORT:-20048}"   # diteruskan ke 20048 (mountd) di VM, harus tertutup
SMTP_PORT="${SMTP_PORT:-12525}"     # server SMTP uji di host (VM: 10.0.2.2), notifikasi email
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
# Disk keempat: disk sistem untuk uji installer (ADR 0016).
truncate -s 20G "$WORK/disk4.img"
disks+=(-drive "file=$WORK/disk4.img,if=none,format=raw,id=d4" -device "virtio-blk-pci,drive=d4,serial=SAPOUJI4")
# Dua drive USB exFAT di satu "dock" murah: nomor seri yang sama untuk
# keduanya (penyimpanan eksternal, ADR 0015).
truncate -s 256M "$WORK/usb1.img" "$WORK/usb2.img"
mkfs.exfat -L FOTO "$WORK/usb1.img" >/dev/null
mkfs.exfat -L ARSIP "$WORK/usb2.img" >/dev/null
disks+=(-device qemu-xhci,id=xhci)
for i in 1 2; do
  disks+=(-drive "file=$WORK/usb$i.img,if=none,format=raw,id=u$i" -device "usb-storage,bus=xhci.0,drive=u$i,serial=SAPODOCK")
done

accel=(-accel tcg)
if [[ -w /dev/kvm ]]; then accel=(-accel kvm -cpu host); fi

qemu-system-x86_64 "${accel[@]}" -m "$VM_RAM_MB" -smp 2 \
  -cdrom "$ISO" -boot d "${disks[@]}" \
  -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$PORT-:5000,hostfwd=tcp:127.0.0.1:$TLS_PORT-:5001,hostfwd=tcp:127.0.0.1:$SMB_PORT-:445,hostfwd=tcp:127.0.0.1:$NFS_PORT-:2049,hostfwd=tcp:127.0.0.1:$APP_PORT-:8090,hostfwd=tcp:127.0.0.1:$RPCBIND_PORT-:111,hostfwd=tcp:127.0.0.1:$MOUNTD_PORT-:20048" \
  -display none \
  -chardev "socket,id=ser0,path=$WORK/serial.sock,server=on,wait=off,logfile=$LOG" -serial chardev:ser0 &
qemu=$!
trap 'sudo umount -f -l "$WORK/nfs" 2>/dev/null || true; kill "$qemu" 2>/dev/null || true; rm -rf "$WORK"' EXIT

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

# Resource Monitor: sapod mulai mengambil sampel saat pertama dibaca.
api GET /api/v1/system/stats >/dev/null
sleep 5
stats="$(api GET /api/v1/system/stats)"
json 's=v["samples"]; assert len(s)>=1, v
x=s[-1]; assert x["memory_total_bytes"]>0 and 0<=x["cpu_percent"]<=100 and x["net"] and x["disk"], x' <<<"$stats" \
  || fail "Resource Monitor: $stats"
echo "Resource Monitor: $(json 's=v["samples"][-1]; print(len(v["samples"]), "sampel, CPU", round(s["cpu_percent"]), "%, jaringan", [n["name"] for n in s["net"]], "disk", [d["name"] for d in s["disk"]])' <<<"$stats")"

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
  [[ "$res" == *' 200' && "$res" != *'"warning":"system_sync_failed"'* ]] || fail "buat pengguna $u: $res; status sinkronisasi: $(api GET /api/v1/system/sync)"
done
res="$(api POST /api/v1/shares "{\"name\":\"keluarga\",\"pool\":\"uji\",\"description\":\"Uji asap\",\"permissions\":{\"user:budi\":\"write\",\"user:ani\":\"read\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "buat shared folder: $res"
json 'assert not v.get("error") and v["path"].endswith("/uji/keluarga"), v' <<<"$res" || fail "shared folder tidak siap: $res; status sinkronisasi: $(api GET /api/v1/system/sync)"
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

# --- Shared folder lewat NFS ----------------------------------------------------
# Koneksi dari host lewat QEMU tampak dari 10.0.2.2 di VM. Semua akses dari
# klien berlaku sebagai pengguna NFS yang dipilih (all_squash), termasuk root.
mkdir -p "$WORK/nfs"
share_path="$(json 'print(v[0]["path"])' <<<"$shares")"
nfs_set() { # nfs_set JSON → respons API
  api POST /api/v1/shares/keluarga "{\"nfs\":$1,\"admin_password\":\"$PASS\"}"
}
nfs_mount() {
  timeout 60 sudo mount -t nfs4 -o "vers=4.2,port=$NFS_PORT,retry=0,soft,timeo=50,retrans=1" "127.0.0.1:$share_path" "$WORK/nfs" 2>&1
}
nfs_umount() { sudo umount "$WORK/nfs"; }
res="$(nfs_set '{"user":"ani","clients":["10.0.2.2"]}')"
[[ "$res" == *' 200' && "$res" != *'system_sync_failed'* ]] || fail "aktifkan NFS: $res; status sinkronisasi: $(api GET /api/v1/system/sync)"
shares="$(api GET /api/v1/shares)"
json 's=v[0]; assert s["nfs"]["clients"]==["10.0.2.2/32"] and s["nfs_access"]=="read" and not s.get("nfs_error"), s' <<<"$shares" \
  || fail "status NFS: $shares"
out="$(nfs_mount)" || fail "pasang NFS (ani, baca): $out"
cmp -s "$WORK/halo.txt" "$WORK/nfs/halo.txt" || fail "NFS sebagai ani (baca) harus bisa membaca halo.txt"
if sudo sh -c "echo x > '$WORK/nfs/coba.txt'" 2>/dev/null; then fail "NFS sebagai ani (baca) tidak boleh menulis, termasuk dari root klien"; fi
nfs_umount
echo "NFS: baca saja sebagai ani, root klien tidak mendapat hak lebih"

# Hanya NFSv4 di port 2049: rpcbind dan mountd tidak menjawab dari jaringan.
# QEMU menerima koneksi di host lalu meneruskannya, jadi yang diperiksa
# adalah jawaban RPC (panggilan NULL), bukan sekadar koneksi.
rpc_answers() { # rpc_answers PORT PROGRAM VERSI → sukses bila ada jawaban
  python3 - "$@" <<'PY'
import socket, struct, sys
port, prog, vers = map(int, sys.argv[1:])
call = struct.pack(">IIIIIIIIII", 0x5A90, 0, 2, prog, vers, 0, 0, 0, 0, 0)
try:
    s = socket.create_connection(("127.0.0.1", port), timeout=5)
    s.sendall(struct.pack(">I", 0x80000000 | len(call)) + call)
    sys.exit(0 if s.recv(64) else 1)
except OSError:
    sys.exit(1)
PY
}
if rpc_answers "$RPCBIND_PORT" 100000 2; then fail "rpcbind tidak boleh terbuka di jaringan (NFS hanya versi 4)"; fi
if rpc_answers "$MOUNTD_PORT" 100005 3; then fail "mountd tidak boleh terbuka di jaringan (NFS hanya versi 4)"; fi
echo "NFS: hanya versi 4; rpcbind dan mountd tidak terbuka"

res="$(nfs_set '{"user":"budi","clients":["10.0.2.0/24"]}')"
[[ "$res" == *' 200' && "$res" != *'system_sync_failed'* ]] || fail "NFS sebagai budi: $res"
out="$(nfs_mount)" || fail "pasang NFS (budi, tulis): $out"
echo "dari NFS" | sudo tee "$WORK/nfs/nfs.txt" >/dev/null || fail "NFS sebagai budi (tulis) harus bisa menulis"
owner="$(stat -c %u "$WORK/nfs/nfs.txt")"
(( owner >= 20000 && owner <= 29999 )) || fail "file dari root klien harus milik akun budi, bukan UID $owner"
nfs_umount
rm -f "$WORK/salinan.txt"
out="$(smb cici "get nfs.txt $WORK/salinan.txt")"
[[ "$(cat "$WORK/salinan.txt" 2>/dev/null)" == "dari NFS" ]] || fail "file dari NFS harus mewarisi izin folder (cici baca lewat SMB): $out"
echo "NFS: tulis sebagai budi (file milik UID $owner), izin folder diwarisi"

# Mencabut izin budi di folder langsung mencabut ekspor NFS-nya.
res="$(api POST /api/v1/shares/keluarga "{\"permissions\":{\"user:ani\":\"read\",\"user:cici\":\"read\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' && "$res" != *'system_sync_failed'* ]] || fail "cabut izin budi: $res"
if out="$(nfs_mount)"; then nfs_umount; fail "NFS sebagai budi tanpa izin tidak boleh bisa dipasang"; fi
res="$(api POST /api/v1/shares/keluarga "{\"permissions\":{\"user:budi\":\"write\",\"user:ani\":\"read\",\"user:cici\":\"read\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "kembalikan izin budi: $res"

res="$(nfs_set '{"user":"budi","clients":["10.0.3.0/24"]}')"
[[ "$res" == *' 200' ]] || fail "NFS klien lain: $res"
if out="$(nfs_mount)"; then nfs_umount; fail "klien di luar jaringan yang diizinkan harus ditolak"; fi
res="$(nfs_set null)"
[[ "$res" == *' 200' ]] || fail "matikan NFS: $res"
if out="$(nfs_mount)"; then nfs_umount; fail "NFS yang dimatikan tidak boleh bisa dipasang"; fi
echo "NFS: klien di luar jaringan dan NFS yang dimatikan ditolak"

# --- File Station (ADR 0010) ------------------------------------------------------
# Lewat panel sebagai budi (tulis) dan ani (baca); izin ditegakkan kernel
# karena sapod menjalankan operasi file sebagai akun pengguna.
admin_jar="$JAR"
login_as() { # login_as NAMA → JAR baru untuk pengguna itu
  JAR="$WORK/cookies-$1"
  res="$(api POST /api/v1/auth/login "{\"username\":\"$1\",\"password\":\"$UPASS\"}")"
  [[ "$res" == *' 200' ]] || fail "login $1: $res"
}
login_as budi
res="$(api GET /api/v1/files/shares)"
json 'assert [s["name"] for s in v if s["access"]=="write"]==["keluarga"], v' <<<"$res" || fail "File Station, folder budi: $res"
res="$(api GET '/api/v1/files/list?share=keluarga&path=')"
json 'assert "halo.txt" in [e["name"] for e in v["entries"]], v' <<<"$res" || fail "File Station, daftar: $res"
curl -ksS -b "$JAR" --max-time 30 -o "$WORK/unduh.txt" "$HTTPS/api/v1/files/download?share=keluarga&path=halo.txt"
cmp -s "$WORK/halo.txt" "$WORK/unduh.txt" || fail "File Station: unduhan halo.txt tidak sama"
echo "dari panel" > "$WORK/panel.txt"
res="$(curl -ksS -b "$JAR" -X POST -H 'X-Sapo-Request: 1' --data-binary "@$WORK/panel.txt" -w ' %{http_code}' --max-time 30 \
  "$HTTPS/api/v1/files/upload?share=keluarga&path=&name=panel.txt")"
[[ "$res" == *' 200' ]] || fail "File Station, unggah: $res"
res="$(api POST /api/v1/files/mkdir '{"share":"keluarga","path":"arsip"}')"
[[ "$res" == *' 200' ]] || fail "File Station, buat folder: $res"
res="$(api POST /api/v1/files/rename '{"share":"keluarga","path":"panel.txt","to":"arsip/panel.txt"}')"
[[ "$res" == *' 200' ]] || fail "File Station, pindah: $res"
# File dari panel milik akun budi dan mewarisi izin folder: cici (baca)
# membukanya lewat SMB.
rm -f "$WORK/salinan.txt"
out="$(smb cici "get arsip/panel.txt $WORK/salinan.txt")"
cmp -s "$WORK/panel.txt" "$WORK/salinan.txt" || fail "file dari File Station harus terbaca cici lewat SMB: $out"
login_as ani
res="$(curl -ksS -b "$JAR" -X POST -H 'X-Sapo-Request: 1' --data-binary "@$WORK/panel.txt" -w ' %{http_code}' --max-time 30 \
  "$HTTPS/api/v1/files/upload?share=keluarga&path=&name=ani.txt")"
[[ "$res" == *'"permission_denied"'*' 403' ]] || fail "File Station: ani (baca) tidak boleh mengunggah: $res"
login_as budi
res="$(api POST /api/v1/files/delete '{"share":"keluarga","path":"arsip"}')"
[[ "$res" == *' 200' ]] || fail "File Station, hapus folder: $res"
res="$(api GET '/api/v1/files/list?share=keluarga&path=')"
json 'assert "arsip" not in [e["name"] for e in v["entries"]], v' <<<"$res" || fail "File Station: arsip harus terhapus: $res"
JAR="$admin_jar"
echo "File Station: daftar, unduh, unggah, folder baru, pindah, hapus sebagai budi; ani (baca) ditolak; file terbaca lewat SMB"

# --- Control Panel ---------------------------------------------------------------
res="$(api POST /api/v1/settings/services '{"smb":false}')"
[[ "$res" == *' 200' && "$res" != *'system_sync_failed'* ]] || fail "matikan SMB: $res"
out="$(smb budi "ls")"
[[ "$out" != *"halo.txt"* ]] || fail "SMB yang dimatikan tidak boleh melayani: $out"
res="$(api POST /api/v1/settings/services '{"smb":true}')"
[[ "$res" == *' 200' ]] || fail "nyalakan SMB: $res"
for _ in $(seq 1 10); do out="$(smb budi "ls")"; [[ "$out" == *"halo.txt"* ]] && break; sleep 2; done
[[ "$out" == *"halo.txt"* ]] || fail "SMB yang dinyalakan lagi harus melayani: $out"
res="$(api POST /api/v1/settings/time '{"timezone":"Asia/Makassar","ntp":true}')"
[[ "$res" == *' 200' ]] || fail "atur zona waktu: $res"
res="$(api GET /api/v1/settings/time)"
json 'assert v["timezone"]=="Asia/Makassar" and v["ntp"], v' <<<"$res" || fail "zona waktu: $res"
echo "Control Panel: SMB dimatikan lalu dinyalakan lagi, zona waktu Asia/Makassar"

# Pengguna yang dinonaktifkan tidak bisa masuk lewat SMB, dan sesinya yang
# sedang terbuka ikut diputus.
# Perintah dikirim lewat stdin dengan jeda, agar sesi tetap terbuka saat ani
# dinonaktifkan.
{ echo ls; sleep 15; echo ls; } | smbclient //127.0.0.1/keluarga -p "$SMB_PORT" -m SMB3 -U "ani%$UPASS" > "$WORK/sesi.txt" 2>&1 &
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

# --- App Center (ADR 0012) --------------------------------------------------------
# Homer (image kecil) dipasang dengan Podman sungguhan: berjalan sebagai akun
# aplikasi tanpa capability, data di dataset uji/sapo-apps/homer.
res="$(api GET /api/v1/apps/catalog)"
json 'assert "homer" in [a["id"] for a in v], v' <<<"$res" || fail "katalog App Center: $res"
res="$(api POST /api/v1/apps/install "{\"id\":\"homer\",\"pool\":\"uji\",\"port\":8090,\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "pasang Homer: $res"
for _ in $(seq 1 100); do
  apps="$(api GET /api/v1/apps)"
  json 'import sys; sys.exit(0 if v and v[0]["status"]=="active" else 1)' <<<"$apps" && \
    curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:$APP_PORT/" 2>/dev/null && break
  sleep 3
done
json 'assert v[0]["status"]=="active", v' <<<"$apps" || fail "Homer tidak berjalan: $apps"
curl -fsS --max-time 5 "http://127.0.0.1:$APP_PORT/" | grep -qi homer || fail "Homer tidak menjawab di port 8090"
res="$(api POST /api/v1/apps/homer/stop '{}')"
[[ "$res" == *' 200' ]] || fail "hentikan Homer: $res"
if curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:$APP_PORT/" 2>/dev/null; then fail "Homer yang dihentikan masih menjawab"; fi
res="$(api POST /api/v1/apps/homer/remove "{\"remove_data\":true,\"confirm_name\":\"homer\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus Homer beserta data: $res"
apps="$(api GET /api/v1/apps)"
json 'assert v==[], v' <<<"$apps" || fail "Homer masih tercatat: $apps"
echo "App Center: Homer dipasang lewat Podman (akun sendiri, tanpa capability), menjawab di port 8090, dihentikan, lalu dihapus beserta data"

# Akses shared folder untuk aplikasi: miniserve (menampilkan folder lewat
# HTTP) hanya diberi folder media (baca saja). File yang sudah ada sebelum
# akses diberikan harus terbaca (ACL akun aplikasi diterapkan ke isi folder),
# folder pribadi tidak pernah terlihat.
for f in media pribadi; do
  res="$(api POST /api/v1/shares "{\"name\":\"$f\",\"pool\":\"uji\",\"permissions\":{\"user:budi\":\"write\"},\"admin_password\":\"$PASS\"}")"
  [[ "$res" == *' 200' ]] || fail "buat folder $f: $res"
  echo "isi $f" > "$WORK/$f.txt"
  out="$(smbclient "//127.0.0.1/$f" -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "put $WORK/$f.txt isi.txt" 2>&1 || true)"
  [[ "$out" != *NT_STATUS* ]] || fail "tulis ke $f: $out"
done
res="$(api POST /api/v1/apps/install "{\"id\":\"miniserve\",\"pool\":\"uji\",\"port\":8090,\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "pasang miniserve: $res"
app_get() { curl -fsS --max-time 5 "http://127.0.0.1:$APP_PORT/$1" 2>/dev/null; }
for _ in $(seq 1 100); do app_get "" >/dev/null && break; sleep 3; done
app_get "" >/dev/null || fail "miniserve tidak berjalan: $(api GET /api/v1/apps)"
res="$(api POST /api/v1/apps/miniserve/folders "{\"folders\":{\"media\":\"write\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 400' ]] || fail "izin tulis untuk aplikasi baca saja harus ditolak: $res"
res="$(api POST /api/v1/apps/miniserve/folders "{\"folders\":{\"media\":\"read\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' && "$res" != *system_sync_failed* ]] || fail "beri folder media: $res; status sinkronisasi: $(api GET /api/v1/system/sync)"
for _ in $(seq 1 40); do [[ "$(app_get media/isi.txt)" == "isi media" ]] && break; sleep 3; done
[[ "$(app_get media/isi.txt)" == "isi media" ]] || fail "miniserve tidak bisa membaca folder media: $(app_get '' | head -c 500)"
if app_get pribadi/isi.txt >/dev/null; then fail "folder pribadi tidak boleh terlihat oleh aplikasi"; fi
res="$(api POST /api/v1/shares/media/delete "{\"confirm_name\":\"media\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *'"share_used_by_app"'*' 409' ]] || fail "folder yang dipakai aplikasi tidak boleh dihapus: $res"
# Dicabut: folder dilepas dari container.
res="$(api POST /api/v1/apps/miniserve/folders "{\"folders\":{},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' && "$res" != *system_sync_failed* ]] || fail "cabut folder media: $res"
for _ in $(seq 1 40); do app_get "" >/dev/null && ! app_get media/isi.txt >/dev/null && break; sleep 3; done
if app_get media/isi.txt >/dev/null; then fail "folder yang dicabut masih terbaca oleh aplikasi"; fi
res="$(api POST /api/v1/apps/miniserve/remove "{\"remove_data\":true,\"confirm_name\":\"miniserve\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus miniserve: $res"
for f in media pribadi; do
  res="$(api POST /api/v1/shares/$f/delete "{\"confirm_name\":\"$f\",\"admin_password\":\"$PASS\"}")"
  [[ "$res" == *' 200' ]] || fail "hapus folder $f: $res"
done
echo "App Center: miniserve membaca folder media yang diberikan (termasuk file lama), tidak melihat folder pribadi, izin tulis ditolak, folder yang dipakai tidak bisa dihapus, dan pencabutan melepas folder"

# --- Sapo Drive (ADR 0013) ---------------------------------------------------------
# Folder pribadi di pool uji (batas 1 GB): budi menulis lewat SMB dan
# membacanya di File Station ("Drive saya"); cici tidak bisa membuka folder
# budi (ani sudah dinonaktifkan di atas). Folder pengguna yang dihapus disimpan dan menahan nama itu sampai
# dihapus admin.
res="$(api POST /api/v1/drive/settings "{\"pool\":\"uji\",\"quota_gb\":1,\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' && "$res" != *system_sync_failed* ]] || fail "nyalakan Sapo Drive: $res; status sinkronisasi: $(api GET /api/v1/system/sync)"
home() { # home USER SAMBUNGAN PERINTAH → keluaran smbclient
  smbclient "//127.0.0.1/$2" -p "$SMB_PORT" -m SMB3 -U "$1%$UPASS" -c "$3" 2>&1 || true
}
echo "milik budi" > "$WORK/pribadi.txt"
out="$(home budi budi "put $WORK/pribadi.txt catatan.txt; ls")"
[[ "$out" == *catatan.txt* ]] || fail "budi menulis ke folder pribadinya: $out"
out="$(home cici budi "ls")"
[[ "$out" == *NT_STATUS_ACCESS_DENIED* || "$out" == *NT_STATUS_BAD_NETWORK_NAME* ]] || fail "cici tidak boleh membuka folder pribadi budi: $out"
out="$(home cici cici "ls")"
[[ "$out" != *NT_STATUS* ]] || fail "cici membuka folder pribadinya: $out"
login_as budi
res="$(api GET '/api/v1/files/list?share=@home&path=')"
json 'assert "catatan.txt" in [e["name"] for e in v["entries"]], v' <<<"$res" || fail "File Station, Drive saya budi: $res"
res="$(api GET /api/v1/drive)"
json 'assert v["home"]["quota_bytes"]==1<<30 and v["home"]["used_bytes"]>0, v' <<<"$res" || fail "pemakaian Drive budi: $res"
JAR="$admin_jar"
# cici dihapus: foldernya disimpan; nama cici tidak bisa dipakai lagi sampai
# foldernya dihapus.
res="$(api POST /api/v1/users/cici/delete "{\"confirm_name\":\"cici\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus pengguna cici: $res"
res="$(api GET /api/v1/drive/admin)"
json 'assert [h["user"] for h in v["orphans"]]==["cici"], v' <<<"$res" || fail "folder sisa cici: $res"
res="$(api POST /api/v1/users "{\"username\":\"cici\",\"password\":\"$UPASS\",\"password_confirm\":\"$UPASS\",\"role\":\"user\",\"groups\":[],\"must_change_password\":false,\"admin_password\":\"$PASS\"}")"
[[ "$res" == *'"home_exists"'*' 409' ]] || fail "nama dengan folder sisa harus ditolak: $res"
res="$(api POST /api/v1/drive/homes/uji/cici/delete "{\"confirm_name\":\"cici\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus folder sisa cici: $res"
res="$(api GET /api/v1/drive/admin)"
json 'assert v["orphans"]==[], v' <<<"$res" || fail "folder sisa masih ada: $res"
echo "Sapo Drive: folder pribadi lewat SMB dan File Station, cici tidak bisa membuka folder budi, kuota 1 GB, folder pengguna yang dihapus disimpan lalu dihapus admin"

# WebDAV (Sapo Drive tahap 2): budi memakai password aplikasi (bukan password
# akun) untuk mengelola Drive saya lewat https://…/dav/; folder baca saja
# tidak bisa ditulis, dan password yang dicabut tidak berlaku lagi.
res="$(api POST /api/v1/shares "{\"name\":\"baca\",\"pool\":\"uji\",\"permissions\":{\"user:budi\":\"read\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "buat folder baca: $res"
login_as budi
res="$(api POST /api/v1/account/app-passwords "{\"name\":\"Uji asap\",\"password\":\"$UPASS\"}")"
[[ "$res" == *' 200' ]] || fail "buat password aplikasi: $res"
APW="$(json 'print(v["password"])' <<<"$res")"
APW_ID="$(json 'print(v["id"])' <<<"$res")"
dav() { # dav METHOD PATH [curl args…] → kode HTTP
  local m="$1" p="$2"; shift 2
  curl -ksS -o "$WORK/dav.out" -w '%{http_code}' --max-time 30 -u "budi:$APW" -X "$m" "$@" "$HTTPS/dav$p"
}
code="$(curl -ksS -o /dev/null -w '%{http_code}' --max-time 30 -u "budi:$UPASS" -X PROPFIND -H 'Depth: 0' "$HTTPS/dav/")"
[[ "$code" == 401 ]] || fail "WebDAV dengan password akun harus ditolak: $code"
code="$(dav PROPFIND / -H 'Depth: 1')"
[[ "$code" == 207 ]] && grep -q 'home' "$WORK/dav.out" && grep -q '/dav/baca/' "$WORK/dav.out" || fail "WebDAV, daftar akar: $code $(head -c 800 "$WORK/dav.out")"
echo "lewat webdav" > "$WORK/dav.txt"
code="$(dav PUT /@home/dav.txt --data-binary "@$WORK/dav.txt")"
[[ "$code" == 201 || "$code" == 204 ]] || fail "WebDAV, unggah: $code $(cat "$WORK/dav.out")"
code="$(dav GET /@home/dav.txt)"
[[ "$code" == 200 ]] && cmp -s "$WORK/dav.txt" "$WORK/dav.out" || fail "WebDAV, unduh: $code"
code="$(dav MKCOL /@home/arsip)"
[[ "$code" == 201 ]] || fail "WebDAV, buat folder: $code $(cat "$WORK/dav.out")"
code="$(dav MOVE /@home/dav.txt -H "Destination: $HTTPS/dav/@home/arsip/dav.txt")"
[[ "$code" == 201 || "$code" == 204 ]] || fail "WebDAV, pindah: $code $(cat "$WORK/dav.out")"
res="$(api GET '/api/v1/files/list?share=@home&path=arsip')"
json 'assert "dav.txt" in [e["name"] for e in v["entries"]], v' <<<"$res" || fail "file dari WebDAV harus tampil di File Station: $res"
code="$(dav PUT /baca/coba.txt --data-binary "@$WORK/dav.txt")"
[[ "$code" == 403 ]] || fail "WebDAV, folder baca saja harus menolak tulis: $code"
code="$(dav DELETE /@home/arsip)"
[[ "$code" == 204 ]] || fail "WebDAV, hapus folder: $code $(cat "$WORK/dav.out")"
res="$(api POST "/api/v1/account/app-passwords/$APW_ID/delete" '{}')"
[[ "$res" == *' 200' ]] || fail "cabut password aplikasi: $res"
code="$(dav PROPFIND / -H 'Depth: 0')"
[[ "$code" == 401 ]] || fail "password aplikasi yang dicabut harus ditolak: $code"
JAR="$admin_jar"
res="$(api POST /api/v1/shares/baca/delete "{\"confirm_name\":\"baca\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus folder baca: $res"
echo "WebDAV: password aplikasi, daftar, unggah, unduh, folder baru, pindah, hapus; password akun, folder baca saja, dan password yang dicabut ditolak"

# --- Snapshot terjadwal (ADR 0014) ------------------------------------------------
# Jadwal per jam untuk folder versi: sapod mengambil snapshot pertama dalam
# satu menit. Snapshot manual, "Previous Versions" lewat SMB, hapus (password
# admin), lalu jadwal dimatikan tanpa menghapus snapshot yang ada.
res="$(api POST /api/v1/shares "{\"name\":\"versi\",\"pool\":\"uji\",\"permissions\":{\"user:budi\":\"write\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "buat folder versi: $res"
echo "versi pertama" > "$WORK/versi.txt"
out="$(smbclient //127.0.0.1/versi -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "put $WORK/versi.txt catatan.txt" 2>&1 || true)"
[[ "$out" != *NT_STATUS* ]] || fail "tulis ke folder versi: $out"
res="$(api POST /api/v1/shares/versi/snapshot-policy '{"policy":{"hourly":2,"daily":0,"weekly":0,"hour":2}}')"
[[ "$res" == *' 200' && "$res" != *system_sync_failed* ]] || fail "jadwal snapshot: $res"
for _ in $(seq 1 50); do
  snaps="$(api GET '/api/v1/snapshots?pool=uji&share=versi')"
  json 'import sys; sys.exit(0 if any(s["kind"]=="hourly" for s in v["snapshots"]) else 1)' <<<"$snaps" && break
  sleep 3
done
json 'assert [s["kind"] for s in v["snapshots"]]==["hourly"], v' <<<"$snaps" || fail "snapshot terjadwal tidak diambil: $snaps"
res="$(api POST /api/v1/snapshots '{"pool":"uji","share":"versi"}')"
[[ "$res" == *' 200' ]] || fail "snapshot manual: $res"
manual="$(json 'print(v["name"])' <<<"$res")"
snaps="$(api GET '/api/v1/snapshots?pool=uji&share=versi')"
json 'assert sorted(s["kind"] for s in v["snapshots"])==["hourly","manual"], v' <<<"$snaps" || fail "daftar snapshot: $snaps"
# Previous Versions (Windows) lewat Samba shadow_copy2.
out="$(smbclient //127.0.0.1/versi -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "allinfo catatan.txt" 2>&1 || true)"
[[ "$out" == *"@GMT-"* ]] || fail "Previous Versions lewat SMB tidak tampil: $out"
res="$(api POST /api/v1/snapshots/delete "{\"pool\":\"uji\",\"share\":\"versi\",\"name\":\"$manual\"}")"
[[ "$res" == *' 401'* || "$res" == *' 400'* ]] || fail "hapus snapshot tanpa password harus ditolak: $res"
res="$(api POST /api/v1/snapshots/delete "{\"pool\":\"uji\",\"share\":\"versi\",\"name\":\"$manual\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus snapshot: $res"
res="$(api POST /api/v1/shares/versi/snapshot-policy '{"policy":null}')"
[[ "$res" == *' 200' ]] || fail "matikan jadwal: $res"
snaps="$(api GET '/api/v1/snapshots?pool=uji&share=versi')"
json 'assert [s["kind"] for s in v["snapshots"]]==["hourly"], v' <<<"$snaps" || fail "snapshot lama harus tetap ada setelah jadwal dimatikan: $snaps"
# Pulihkan dari File Station (sebagai budi): file yang dihapus kembali dengan
# isi versi snapshot; bila nama sudah ada, hasilnya bernama baru; pengguna
# baca saja (eko) tidak bisa memulihkan.
snap_name="$(json 'print(v["snapshots"][0]["name"])' <<<"$snaps")"
res="$(api POST /api/v1/users "{\"username\":\"eko\",\"password\":\"$UPASS\",\"password_confirm\":\"$UPASS\",\"role\":\"user\",\"groups\":[],\"must_change_password\":false,\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "buat pengguna eko: $res"
res="$(api POST /api/v1/shares/versi "{\"permissions\":{\"user:budi\":\"write\",\"user:eko\":\"read\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "izin folder versi: $res"
login_as budi
res="$(api GET '/api/v1/files/snapshots?share=versi')"
json "assert '$snap_name' in [s['name'] for s in v], v" <<<"$res" || fail "versi sebelumnya di File Station: $res"
res="$(api GET "/api/v1/files/list?share=versi&path=&snapshot=$snap_name")"
json 'assert "catatan.txt" in [e["name"] for e in v["entries"]], v' <<<"$res" || fail "isi versi sebelumnya: $res"
res="$(api POST /api/v1/files/delete '{"share":"versi","path":"catatan.txt"}')"
[[ "$res" == *' 200' ]] || fail "hapus catatan.txt: $res"
res="$(api POST /api/v1/files/restore "{\"share\":\"versi\",\"path\":\"catatan.txt\",\"snapshot\":\"$snap_name\"}")"
[[ "$res" == *'"name":"catatan.txt"'*' 200' ]] || fail "pulihkan file yang dihapus: $res"
curl -ksS -b "$JAR" --max-time 30 -o "$WORK/pulih.txt" "$HTTPS/api/v1/files/download?share=versi&path=catatan.txt"
cmp -s "$WORK/versi.txt" "$WORK/pulih.txt" || fail "isi file yang dipulihkan tidak sama"
res="$(api POST /api/v1/files/restore "{\"share\":\"versi\",\"path\":\"catatan.txt\",\"snapshot\":\"$snap_name\"}")"
[[ "$res" == *'"name":"catatan (dipulihkan '*' 200' ]] || fail "pulihkan ke nama yang sudah ada harus bernama baru: $res"
login_as eko
res="$(api POST /api/v1/files/restore "{\"share\":\"versi\",\"path\":\"catatan.txt\",\"snapshot\":\"$snap_name\"}")"
[[ "$res" == *' 403' ]] || fail "eko (baca saja) tidak boleh memulihkan: $res"
JAR="$admin_jar"
echo "Pulihkan dari snapshot: daftar versi, isi versi lama, file yang dihapus kembali utuh, nama baru bila sudah ada, pengguna baca saja ditolak"
res="$(api POST /api/v1/shares/versi/delete "{\"confirm_name\":\"versi\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus folder versi: $res"
echo "Snapshot: terjadwal per jam, manual, Previous Versions lewat SMB, hapus dengan password, jadwal dimatikan tanpa menghapus snapshot"

# --- Penyimpanan eksternal (USB, ADR 0015) ----------------------------------------
# Dua drive exFAT di "dock" yang melaporkan nomor seri sama: tidak bisa dipakai
# pool (USB, lalu identitas tidak unik). FOTO dipasang baca-tulis untuk semua
# pengguna (budi menulis lewat SMB, isinya tetap ada setelah dilepas dan
# dipasang lagi); ARSIP baca saja untuk grup tamu (eko boleh membaca tetapi
# tidak menulis, budi tidak melihatnya).
for _ in $(seq 1 20); do
  ext="$(api GET /api/v1/storage/external)"
  json 'import sys; sys.exit(0 if sorted(x["label"] for x in v if x["supported"])==["ARSIP","FOTO"] else 1)' <<<"$ext" && break
  sleep 3
done
json 'assert sorted(x["label"] for x in v if x["supported"])==["ARSIP","FOTO"], v' <<<"$ext" || fail "drive USB tidak terbaca: $ext"
uuid_of() { json "print([x['uuid'] for x in v if x['label']=='$1'][0])" <<<"$ext"; }
disks_json="$(api GET /api/v1/storage/disks)"
usbref="$(json "
d=[x for x in v if x['serial']=='SAPODOCK'][0]
import json as j; print(j.dumps([dict(name=d['name'],serial=d['serial'],wwn=d['wwn'],size_bytes=d['size_bytes'],usage=d['usage'],erase=True)]))" <<<"$disks_json")"
res="$(api POST /api/v1/storage/pools "{\"name\":\"usb\",\"layout\":\"single\",\"disks\":$usbref,\"confirm_name\":\"usb\",\"password\":\"$PASS\"}")"
[[ "$res" == *'"disk_usb"'* ]] || fail "disk USB tanpa izin harus ditolak untuk pool: $res"
res="$(api POST /api/v1/storage/pools "{\"name\":\"usb\",\"layout\":\"single\",\"disks\":$usbref,\"confirm_name\":\"usb\",\"password\":\"$PASS\",\"allow_usb\":true}")"
[[ "$res" == *'"disk_identity"'* ]] || fail "disk dengan nomor seri kembar harus ditolak: $res"
res="$(api POST /api/v1/storage/external/mount "{\"uuid\":\"$(uuid_of FOTO)\",\"name\":\"usb-foto\",\"group\":\"*\",\"read_only\":false,\"smb\":true,\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "pasang FOTO: $res"
res="$(api POST /api/v1/groups '{"name":"tamu"}')"
[[ "$res" == *' 200' ]] || fail "buat grup tamu: $res"
res="$(api POST /api/v1/groups/tamu "{\"members\":[\"eko\"],\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "anggota grup tamu: $res"
res="$(api POST /api/v1/storage/external/mount "{\"uuid\":\"$(uuid_of ARSIP)\",\"name\":\"usb-arsip\",\"group\":\"tamu\",\"read_only\":true,\"smb\":true,\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "pasang ARSIP: $res"
echo "di flashdisk" > "$WORK/usb.txt"
out="$(smbclient //127.0.0.1/usb-foto -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "put $WORK/usb.txt catatan.txt; ls" 2>&1 || true)"
[[ "$out" == *catatan.txt* ]] || fail "budi menulis ke drive USB lewat SMB: $out"
login_as budi
res="$(api GET /api/v1/files/shares)"
json 'n=[s["name"] for s in v]; assert "usb-foto" in n and "usb-arsip" not in n, v' <<<"$res" || fail "folder eksternal budi: $res"
res="$(api GET '/api/v1/files/list?share=usb-foto&path=')"
json 'assert "catatan.txt" in [e["name"] for e in v["entries"]], v' <<<"$res" || fail "File Station, drive USB: $res"
login_as eko
res="$(api GET '/api/v1/files/list?share=usb-arsip&path=')"
[[ "$res" == *' 200' ]] || fail "eko membaca ARSIP: $res"
out="$(smbclient //127.0.0.1/usb-arsip -p "$SMB_PORT" -m SMB3 -U "eko%$UPASS" -c "put $WORK/usb.txt coba.txt" 2>&1 || true)"
[[ "$out" == *NT_STATUS_ACCESS_DENIED* || "$out" == *NT_STATUS_MEDIA_WRITE_PROTECTED* ]] || fail "ARSIP baca saja tidak boleh ditulis: $out"
JAR="$admin_jar"
# Dilepas dengan aman, lalu dipasang lagi: isinya tetap ada.
for n in usb-foto usb-arsip; do
  res="$(api POST "/api/v1/storage/external/$n/unmount" '{}')"
  [[ "$res" == *' 200' ]] || fail "lepas $n: $res"
done
out="$(smbclient //127.0.0.1/usb-foto -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c ls 2>&1 || true)"
[[ "$out" == *NT_STATUS_BAD_NETWORK_NAME* ]] || fail "folder eksternal yang dilepas masih dibagikan: $out"
ext="$(api GET /api/v1/storage/external)"
res="$(api POST /api/v1/storage/external/mount "{\"uuid\":\"$(uuid_of FOTO)\",\"name\":\"usb-foto\",\"group\":\"*\",\"read_only\":true,\"smb\":false,\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "pasang FOTO lagi: $res"
login_as budi
res="$(api GET '/api/v1/files/list?share=usb-foto&path=')"
json 'assert "catatan.txt" in [e["name"] for e in v["entries"]], v' <<<"$res" || fail "isi drive setelah dipasang lagi: $res"
JAR="$admin_jar"
res="$(api POST /api/v1/storage/external/usb-foto/unmount '{}')"
[[ "$res" == *' 200' ]] || fail "lepas usb-foto: $res"
echo "Penyimpanan eksternal: dua drive USB di satu dock, ditolak untuk pool (USB, nomor seri kembar), dipasang untuk semua/grup, baca-tulis dan baca saja lewat SMB dan File Station, dilepas dan dipasang lagi tanpa kehilangan isi"

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
# Panel ditunggu sampai menjawab lagi (setelah jaringan atau panel
# dinyalakan ulang).
wait_panel() { # wait_panel DETIK
  local deadline=$((SECONDS + $1))
  until curl -kfsS --max-time 5 "$HTTPS/api/v1/health" 2>/dev/null | grep -q '"agent":true'; do
    (( SECONDS <= deadline )) || return 1
    sleep 3
  done
}

# --- Pengaturan jaringan dari panel -----------------------------------------------
# Alamat tetap yang sama dengan DHCP QEMU (10.0.2.15): dikonfirmasi dan
# disimpan. Lalu alamat lain (10.0.2.99): panel tidak terjangkau lagi lewat
# QEMU, jadi tidak ada konfirmasi dan sapod mengembalikan pengaturan lama.
net="$(api GET /api/v1/network)"
iface="$(json 'print([l["name"] for l in v["interfaces"] if "10.0.2.15/24" in l["addresses"]][0])' <<<"$net")" \
  || fail "status jaringan: $net"
net_apply() { # net_apply ALAMAT → token
  local res
  res="$(api POST /api/v1/network "{\"config\":{\"hostname\":\"sapo-uji\",\"interfaces\":[{\"name\":\"$iface\",\"mode\":\"static\",\"address\":\"$1\",\"gateway\":\"10.0.2.2\"}],\"dns\":[\"10.0.2.3\"]},\"admin_password\":\"$PASS\"}")"
  [[ "$res" == *' 200' ]] || fail "terapkan jaringan $1: $res"
  json 'print(v["token"])' <<<"$res"
}
token="$(net_apply 10.0.2.15/24)"
for _ in $(seq 1 30); do
  res="$(api POST /api/v1/network/confirm "{\"token\":\"$token\"}" || true)"
  [[ "$res" == *' 200' ]] && break
  sleep 3
done
[[ "$res" == *' 200' ]] || fail "konfirmasi jaringan: $res"
net="$(api GET /api/v1/network)"
json "c=v['config']; i=[x for x in c['interfaces'] if x['name']=='$iface'][0]
assert v['hostname']=='sapo-uji' and c['hostname']=='sapo-uji' and i['mode']=='static' and i['address']=='10.0.2.15/24', v
assert '10.0.2.3' in v['dns'] and not v.get('pending'), v" <<<"$net" || fail "pengaturan jaringan tersimpan: $net"
sleep 6 # nama host dibaca ulang sapo-api setiap beberapa detik
res="$(curl -ksS -H 'Host: sapo-uji.local' -w ' %{http_code}' --max-time 5 "$HTTPS/api/v1/health")"
[[ "$res" == *' 200' ]] || fail "panel harus bisa dibuka lewat nama host baru (sapo-uji.local): $res"
echo "jaringan: alamat tetap $iface 10.0.2.15/24 dan nama host sapo-uji dikonfirmasi (sapo-uji.local diterima)"

net_apply 10.0.2.99/24 >/dev/null
sleep 15
curl -kfsS --max-time 5 "$HTTPS/api/v1/health" >/dev/null 2>&1 && fail "panel seharusnya tidak terjangkau di alamat lama setelah alamat diganti"
wait_panel 240 || fail "pengaturan jaringan yang tidak dikonfirmasi tidak dikembalikan"
net="$(api GET /api/v1/network)"
json "c=v['config']; i=[x for x in c['interfaces'] if x['name']=='$iface'][0]
assert i['address']=='10.0.2.15/24' and not v.get('pending'), v
assert any('10.0.2.15/24' in l['addresses'] for l in v['interfaces']), v" <<<"$net" || fail "pengaturan lama harus kembali: $net"
echo "jaringan: alamat 10.0.2.99 tidak dikonfirmasi, pengaturan lama kembali sendiri"

# --- Pembaruan sistem (ADR 0011) ---------------------------------------------------
# Periksa selalu (apt-get update di VM sungguhan). Pasang hanya bila daftarnya
# kecil dan tidak menyentuh kernel, ZFS, atau Sapo sendiri (paket Sapo dari
# repo bisa berbeda dengan yang sedang diuji), agar uji asap tidak lama.
res="$(curl -ksS -b "$JAR" -X POST -H 'X-Sapo-Request: 1' -w ' %{http_code}' --max-time 300 "$HTTPS/api/v1/updates/check")"
[[ "$res" == *' 200' ]] || fail "periksa pembaruan: $res"
pkgs="$(json 'print(" ".join(p["name"] for p in v["packages"]))' <<<"$res")"
count="$(json 'print(len(v["packages"]))' <<<"$res")"
echo "pembaruan: $count paket bisa diperbarui"
# Hasilnya dicatat untuk Control Panel dan notifikasi; jadwal harian bisa
# diubah dan dimatikan.
res="$(api GET /api/v1/updates/schedule)"
json "assert not v['off'] and v['hour']==4 and len(v['last']['packages'])==$count, v" <<<"$res" || fail "hasil pemeriksaan tercatat: $res"
res="$(api POST /api/v1/updates/schedule '{"off":true,"hour":3}')"
[[ "$res" == *' 200' ]] || fail "matikan pemeriksaan otomatis: $res"
res="$(api GET /api/v1/updates/schedule)"
json "assert v['off'] and v['hour']==3, v" <<<"$res" || fail "jadwal pemeriksaan: $res"
res="$(api POST /api/v1/updates/schedule '{"off":false,"hour":4}')"
[[ "$res" == *' 200' ]] || fail "nyalakan pemeriksaan otomatis: $res"
echo "pembaruan: hasil pemeriksaan tercatat, jadwal harian diubah, dimatikan, dan dinyalakan lagi"
if (( count <= 15 )) && ! grep -qE '(^| )(linux-|zfs|sapo( |$))' <<<"$pkgs"; then
  res="$(api POST /api/v1/updates/install "{\"admin_password\":\"$PASS\"}")"
  [[ "$res" == *' 200' ]] || fail "pasang pembaruan: $res"
  for _ in $(seq 1 200); do
    st="$(api GET /api/v1/updates || true)"
    json 'import sys; sys.exit(0 if not v["running"] and (v.get("last") or {}).get("finished_at") else 1)' <<<"$st" 2>/dev/null && break
    sleep 3
  done
  json 'assert v["last"]["ok"], v' <<<"$st" || fail "pemasangan pembaruan: $st"
  echo "pembaruan: dipasang lewat unit sapo-updates ($(json 'print(v["log"][-1])' <<<"$st"))"
else
  echo "pembaruan: pemasangan dilewati di uji asap (daftar besar atau menyentuh kernel/ZFS/Sapo): $pkgs" | cut -c1-300
fi

# --- Installer ke disk (ADR 0016) ---------------------------------------------------
# Pool dan shared folder "tetap" dibuat dulu (pool uji sudah dihapus di atas),
# lalu Sapo dipasang ke SAPOUJI4. Di akhir uji, VM dinyalakan ulang dari disk
# itu tanpa ISO.
disks_json="$(api GET /api/v1/storage/disks)"
res="$(api POST /api/v1/storage/pools "{\"name\":\"tetap\",\"layout\":\"single\",\"disks\":$(refs "('SAPOUJI1',True)"),\"confirm_name\":\"tetap\",\"password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "pool tetap: $res"
res="$(api POST /api/v1/shares "{\"name\":\"tetap\",\"pool\":\"tetap\",\"permissions\":{\"user:budi\":\"write\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "folder tetap: $res"
echo "tersimpan di disk" > "$WORK/tetap.txt"
out="$(smbclient //127.0.0.1/tetap -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "put $WORK/tetap.txt catatan.txt" 2>&1 || true)"
[[ "$out" != *NT_STATUS* ]] || fail "tulis ke folder tetap: $out"

# --- Backup ke pool lain (ADR 0017) -------------------------------------------------
# Folder tetap di-backup ke pool cadangan (SAPOUJI2): penuh, lalu hanya
# perubahannya. Folder dan pool yang dipakai tugas tidak bisa dihapus.
# Salinan pertama dipulihkan sebagai folder baru dan dibaca budi lewat SMB.
disks_json="$(api GET /api/v1/storage/disks)"
res="$(api POST /api/v1/storage/pools "{\"name\":\"cadangan\",\"layout\":\"single\",\"disks\":$(refs "('SAPOUJI2',True)"),\"confirm_name\":\"cadangan\",\"password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "pool cadangan: $res"
res="$(api POST /api/v1/backups '{"share":"tetap","dest_pool":"tetap","hour":3,"keep":5,"enabled":true,"admin_password":"'"$PASS"'"}')"
[[ "$res" == *'"backup_same_pool"'* ]] || fail "backup ke pool yang sama harus ditolak: $res"
# Jadwal mati agar penjadwal tidak berebut dengan "backup sekarang".
res="$(api POST /api/v1/backups '{"share":"tetap","dest_pool":"cadangan","hour":3,"keep":5,"enabled":false,"admin_password":"'"$PASS"'"}')"
[[ "$res" == *' 200' ]] || fail "buat tugas backup: $res"
bk_id="tetap-cadangan"
backup_now() { # backup_now JUMLAH_SALINAN
  local res
  res="$(api POST "/api/v1/backups/$bk_id/run" '{}')"
  [[ "$res" == *' 200' ]] || fail "backup sekarang: $res"
  for _ in $(seq 1 60); do
    bk="$(api GET /api/v1/backups)"
    json "import sys; j=v['jobs'][0]; sys.exit(0 if not j['running'] and len(j['copies'])==$1 else 1)" <<<"$bk" && return 0
    sleep 2
  done
  fail "backup tidak selesai: $bk"
}
backup_now 1
json 'j=v["jobs"][0]; assert j["last_ok"] and j["dest_exists"] and j["last_bytes"]>0, j' <<<"$bk" || fail "backup pertama: $bk"
full_bytes="$(json 'print(v["jobs"][0]["last_bytes"])' <<<"$bk")"
echo "sesudah backup pertama" > "$WORK/baru.txt"
out="$(smbclient //127.0.0.1/tetap -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "put $WORK/baru.txt baru.txt" 2>&1 || true)"
[[ "$out" != *NT_STATUS* ]] || fail "tulis baru.txt: $out"
backup_now 2
json "j=v['jobs'][0]; assert j['last_ok'] and j['last_bytes'] > 0, j" <<<"$bk" || fail "backup kedua (perubahan saja): $bk"
res="$(api POST /api/v1/storage/pools/cadangan/destroy "{\"confirm_name\":\"cadangan\",\"password\":\"$PASS\"}")"
[[ "$res" == *' 409' ]] || fail "pool tujuan backup tidak boleh dihapus: $res"
res="$(api POST /api/v1/shares/tetap/delete "{\"confirm_name\":\"tetap\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 409' ]] || fail "folder yang di-backup tidak boleh dihapus: $res"
first="$(json 'print(v["jobs"][0]["copies"][0]["name"])' <<<"$bk")"
res="$(api POST "/api/v1/backups/$bk_id/restore" "{\"copy\":\"$first\",\"name\":\"tetap\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *'"share_taken"'* ]] || fail "pemulihan tidak boleh menimpa folder yang ada: $res"
res="$(api POST "/api/v1/backups/$bk_id/restore" "{\"copy\":\"$first\",\"name\":\"pulih\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "pulihkan: $res"
for _ in $(seq 1 60); do
  bk="$(api GET /api/v1/backups)"
  json 'import sys; r=v["restores"][-1]; sys.exit(0 if not r["running"] else 1)' <<<"$bk" && break
  sleep 2
done
json 'r=v["restores"][-1]; assert r["ok"] and r["present"], r' <<<"$bk" || fail "pemulihan: $bk"
res="$(api GET /api/v1/shares)"
json 'p=[s for s in v if s["name"]=="pulih"][0]; assert p["pool"]=="tetap" and p["permissions"]=={"user:budi":"write"}, p' <<<"$res" || fail "folder pulihan: $res"
for _ in $(seq 1 10); do
  out="$(smbclient //127.0.0.1/pulih -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "get catatan.txt $WORK/pulih.txt; ls" 2>&1 || true)"
  [[ "$out" == *catatan.txt* ]] && break
  sleep 2
done
cmp -s "$WORK/pulih.txt" "$WORK/tetap.txt" || fail "isi folder pulihan lewat SMB: $out"
[[ "$out" != *baru.txt* ]] || fail "salinan pertama tidak boleh berisi file sesudahnya: $out"
# --- Backup ke NAS lain (ADR 0019) ---------------------------------------------------
# NAS ini berperan sebagai pengirim dan penerima (127.0.0.1:5001): partner
# dibuat di pool cadangan, kode pemasangan dipakai untuk memasangkan, folder
# tetap dikirim penuh lalu inkremental lewat HTTPS, lalu salinan pertama
# dipulihkan sebagai folder baru dan dibaca lewat SMB.
res="$(api POST /api/v1/backup-partners '{"name":"NAS uji","pool":"cadangan","keep":3,"admin_password":"'"$PASS"'"}')"
[[ "$res" == *' 200' ]] || fail "buat partner: $res"
partner="$(json 'print(v["id"])' <<<"$res")"
code="$(json 'print(v["code"])' <<<"$res")"
last="${code: -1}"; [[ "$last" == 0 ]] && other=1 || other=0
res="$(api POST /api/v1/remote-backups/targets '{"name":"NAS uji","address":"127.0.0.1:5001","code":"'"${code%?}$other"'","admin_password":"'"$PASS"'"}')"
[[ "$res" == *'"pairing_failed"'* ]] || fail "kode dengan sidik jari lain harus ditolak: $res"
res="$(api POST /api/v1/remote-backups/targets '{"name":"NAS uji","address":"127.0.0.1:5001","code":"'"$code"'","admin_password":"'"$PASS"'"}')"
[[ "$res" == *' 200' ]] || fail "pasangkan NAS tujuan: $res"
res="$(api POST /api/v1/remote-backups/jobs '{"share":"tetap","target":"'"$partner"'","hour":3,"enabled":false,"admin_password":"'"$PASS"'"}')"
[[ "$res" == *' 200' ]] || fail "tugas ke NAS lain: $res"
rjob="$(json 'print(v["id"])' <<<"$res")"
remote_now() { # remote_now JUMLAH_SALINAN
  local res
  res="$(api POST "/api/v1/remote-backups/jobs/$rjob/run" '{}')"
  [[ "$res" == *' 200' ]] || fail "backup ke NAS lain sekarang: $res"
  for _ in $(seq 1 60); do
    rb="$(api GET /api/v1/remote-backups)"
    json "import sys; j=v['jobs'][0]; sys.exit(0 if not j['running'] and (j.get('last_copies')==$1 or j.get('last_error')) else 1)" <<<"$rb" && break
    sleep 2
  done
  json "j=v['jobs'][0]; assert j['last_ok'] and j['last_copies']==$1 and j['last_bytes']>0, j" <<<"$rb" || fail "backup ke NAS lain ($1): $rb"
}
remote_now 1
echo "ke NAS lain" > "$WORK/jauh.txt"
out="$(smbclient //127.0.0.1/tetap -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "put $WORK/jauh.txt jauh.txt" 2>&1 || true)"
[[ "$out" != *NT_STATUS* ]] || fail "tulis jauh.txt: $out"
remote_now 2
res="$(api GET /api/v1/backup-partners)"
json 'j=v[0]["jobs"][0]; assert len(j["copies"])==2 and j["used_bytes"]>0, v' <<<"$res" || fail "salinan di penerima: $res"
rfirst="$(json 'print(v[0]["jobs"][0]["copies"][0]["name"])' <<<"$res")"
res="$(api POST /api/v1/backup-partners/$partner/jobs/$rjob/restore '{"copy":"'"$rfirst"'","name":"dari-nas","admin_password":"'"$PASS"'"}')"
[[ "$res" == *' 200' ]] || fail "pulihkan salinan dari NAS lain: $res"
for _ in $(seq 1 60); do
  bk="$(api GET /api/v1/backups)"
  json 'import sys; r=v["restores"][-1]; sys.exit(0 if r["name"]=="dari-nas" and not r["running"] else 1)' <<<"$bk" && break
  sleep 2
done
json 'r=v["restores"][-1]; assert r["ok"] and r["present"], r' <<<"$bk" || fail "pemulihan dari NAS lain: $bk"
res="$(api GET /api/v1/shares)"
json 'p=[s for s in v if s["name"]=="dari-nas"][0]; assert p["pool"]=="cadangan" and p["permissions"]=={}, p' <<<"$res" || fail "folder dari NAS lain: $res"
res="$(api POST /api/v1/shares/dari-nas "{\"permissions\":{\"user:budi\":\"read\"},\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "izin folder dari NAS lain: $res"
for _ in $(seq 1 10); do
  out="$(smbclient //127.0.0.1/dari-nas -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "get catatan.txt $WORK/dari-nas.txt; ls" 2>&1 || true)"
  [[ "$out" == *catatan.txt* ]] && break
  sleep 2
done
cmp -s "$WORK/dari-nas.txt" "$WORK/tetap.txt" || fail "isi folder dari NAS lain lewat SMB: $out"
[[ "$out" != *jauh.txt* ]] || fail "salinan pertama tidak boleh berisi file sesudahnya: $out"
res="$(api POST /api/v1/storage/pools/cadangan/destroy "{\"confirm_name\":\"cadangan\",\"password\":\"$PASS\"}")"
[[ "$res" == *' 409' ]] || fail "pool berisi backup dari NAS lain tidak boleh dihapus: $res"
# Bersih-bersih: folder pulihan, tugas, NAS tujuan, partner beserta salinan.
res="$(api POST /api/v1/shares/dari-nas/delete "{\"confirm_name\":\"dari-nas\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus folder dari-nas: $res"
res="$(api POST "/api/v1/remote-backups/jobs/$rjob/delete" '{"admin_password":"'"$PASS"'"}')"
[[ "$res" == *' 200' ]] || fail "hapus tugas ke NAS lain: $res"
res="$(api POST "/api/v1/remote-backups/targets/$partner/delete" '{}')"
[[ "$res" == *' 200' ]] || fail "hapus NAS tujuan: $res"
res="$(api POST /api/v1/backup-partners/$partner/delete '{"delete_data":true,"confirm_name":"NAS uji","admin_password":"'"$PASS"'"}')"
[[ "$res" == *' 200' ]] || fail "hapus partner beserta salinan: $res"
echo "Backup ke NAS lain: dipasangkan dengan kode (sidik jari lain ditolak), penuh lalu inkremental lewat HTTPS ($(json 'print(v["jobs"][0]["last_bytes"])' <<<"$rb") byte terakhir), salinan dipulihkan di penerima dan terbaca lewat SMB, pool penerima tidak bisa dihapus selama ada salinan"
res="$(api POST "/api/v1/backups/$bk_id/delete" "{\"delete_data\":true,\"confirm_name\":\"$bk_id\",\"admin_password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus tugas backup: $res"
res="$(api POST /api/v1/storage/pools/cadangan/destroy "{\"confirm_name\":\"cadangan\",\"password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "hapus pool cadangan setelah tugas dihapus: $res"
echo "Backup: folder tetap ke pool cadangan (penuh $full_bytes byte, lalu inkremental), pool dan folder yang dipakai tidak bisa dihapus, salinan pertama dipulihkan sebagai folder pulih dan terbaca lewat SMB"

# --- Notifikasi email (ADR 0018) -----------------------------------------------------
# Server SMTP kecil di host (VM melihatnya di 10.0.2.2) menyimpan setiap email
# (subjek dan isi yang sudah didekode). Email uji, lalu ambang pool penuh
# diturunkan ke 1% (pool tetap terisi) → email masalah, dinaikkan lagi →
# email pulih.
mkdir -p "$WORK/mail"
cat >"$WORK/smtp-sink.py" <<'PYEOF'
import email, email.policy, socketserver, sys
out = sys.argv[2]
class H(socketserver.StreamRequestHandler):
    def handle(self):
        say = lambda s: (self.wfile.write((s + "\r\n").encode()), self.wfile.flush())
        say("220 uji")
        while True:
            line = self.rfile.readline()
            if not line:
                return
            cmd = line.decode(errors="replace").strip().split(" ")[0].upper()
            if cmd in ("EHLO", "HELO"):
                say("250 uji")
            elif cmd in ("MAIL", "RCPT", "RSET", "NOOP"):
                say("250 OK")
            elif cmd == "DATA":
                say("354 lanjut")
                data = b""
                while True:
                    l = self.rfile.readline()
                    if not l or l == b".\r\n":
                        break
                    data += l[1:] if l.startswith(b"..") else l
                m = email.message_from_bytes(data, policy=email.policy.default)
                import os, time
                n = len(os.listdir(out))
                with open(f"{out}/{n:03d}.txt", "w") as f:
                    f.write("Subject: " + str(m["subject"]) + "\nTo: " + str(m["to"]) + "\n\n" + m.get_content())
                say("250 diterima")
            elif cmd == "QUIT":
                say("221 bye")
                return
            else:
                say("502 ?")
socketserver.ThreadingTCPServer.allow_reuse_address = True
socketserver.ThreadingTCPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PYEOF
python3 "$WORK/smtp-sink.py" "$SMTP_PORT" "$WORK/mail" &
smtp_sink=$!
wait_mail() { # wait_mail POLA → isi email terbaru yang memuat POLA
  local f
  for _ in $(seq 1 30); do
    f="$(grep -l -- "$1" "$WORK"/mail/*.txt 2>/dev/null | tail -1 || true)"
    [[ -n "$f" ]] && { cat "$f"; return 0; }
    sleep 2
  done
  fail "email \"$1\" tidak datang: $(cat "$WORK"/mail/*.txt 2>/dev/null)"
}
notify_set() { # notify_set AMBANG
  api POST /api/v1/notify '{"enabled":true,"host":"10.0.2.2","port":'"$SMTP_PORT"',"security":"none","from":"sapo@uji.local","to":["admin@uji.local"],"capacity_pct":'"$1"',"admin_password":"'"$PASS"'"}'
}
res="$(api POST /api/v1/notify '{"enabled":true,"host":"10.0.2.2","port":'"$SMTP_PORT"',"security":"none","username":"admin","password":"x","from":"sapo@uji.local","to":["admin@uji.local"],"admin_password":"'"$PASS"'"}')"
[[ "$res" == *'"smtp_invalid"'* ]] || fail "login SMTP tanpa enkripsi harus ditolak: $res"
res="$(notify_set 90)"
[[ "$res" == *' 200' ]] || fail "atur notifikasi: $res"
res="$(api GET /api/v1/notify)"
json 'assert v["settings"]["enabled"] and v["settings"]["to"]==["admin@uji.local"] and not v["has_password"], v' <<<"$res" || fail "pengaturan notifikasi: $res"
res="$(api POST /api/v1/notify/test '{}')"
[[ "$res" == *' 200' ]] || fail "email uji: $res"
mail_txt="$(wait_mail "Email uji dari Sapo")"
[[ "$mail_txt" == *"To: admin@uji.local"* && "$mail_txt" == *"notifikasi email sudah bekerja"* ]] || fail "isi email uji: $mail_txt"
# Pool tetap (2 GB) diisi file acak 60 MB agar terisi minimal 1%.
head -c $((60 * 1024 * 1024)) /dev/urandom > "$WORK/besar.bin"
out="$(smbclient //127.0.0.1/tetap -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "put $WORK/besar.bin besar.bin" 2>&1 || true)"
[[ "$out" != *NT_STATUS* ]] || fail "tulis besar.bin: $out"
for _ in $(seq 1 20); do
  pools="$(api GET /api/v1/storage/pools)"
  json 'import sys; sys.exit(0 if [p for p in v["pools"] if p["name"]=="tetap"][0]["capacity_pct"]>=1 else 1)' <<<"$pools" && break
  sleep 2
done
res="$(notify_set 1)"
[[ "$res" == *' 200' ]] || fail "ambang 1%: $res"
mail_txt="$(wait_mail "masalah perlu perhatian")"
[[ "$mail_txt" == *"Pool tetap sudah terisi"* ]] || fail "email pool hampir penuh: $mail_txt"
res="$(api GET /api/v1/notify)"
json 'assert any(a["event"]=="pool_full" and a["name"]=="tetap" for a in v["alerts"]), v' <<<"$res" || fail "masalah aktif di panel: $res"
res="$(notify_set 90)"
[[ "$res" == *' 200' ]] || fail "ambang 90%: $res"
mail_txt="$(wait_mail "Masalah sudah pulih")"
[[ "$mail_txt" == *"Sudah pulih"*"Pool tetap"* ]] || fail "email pulih: $mail_txt"
res="$(api POST /api/v1/notify '{"enabled":false,"host":"10.0.2.2","port":'"$SMTP_PORT"',"security":"none","from":"sapo@uji.local","to":["admin@uji.local"],"admin_password":"'"$PASS"'"}')"
[[ "$res" == *' 200' ]] || fail "matikan notifikasi: $res"
kill "$smtp_sink" 2>/dev/null || true
smbclient //127.0.0.1/tetap -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "del besar.bin" >/dev/null 2>&1 || true
echo "Notifikasi email: email uji, pool hampir penuh, lalu pulih diterima server SMTP di host ($(ls "$WORK"/mail | wc -l | tr -d ' ') email); login tanpa enkripsi ditolak"
res="$(api GET /api/v1/system/install)"
json 'assert v["live"] and not v["running"], v' <<<"$res" || fail "installer harus tersedia di ISO: $res"
sysref="$(json "
d=[x for x in v if x['serial']=='SAPOUJI4'][0]
import json as j; print(j.dumps(dict(name=d['name'],serial=d['serial'],wwn=d['wwn'],size_bytes=d['size_bytes'],usage=d['usage'],erase=d['usage']!='free')))" <<<"$disks_json")"
# (json membuang kata terakhir, biasanya kode HTTP: diberi kode tiruan.)
sysdisk="$(json 'print(v["name"])' <<<"$sysref 0")"
res="$(api POST /api/v1/system/install "{\"disk\":$sysref,\"confirm_name\":\"$sysdisk\",\"password\":\"$PASS\"}")"
[[ "$res" == *' 200' ]] || fail "mulai pasang ke disk: $res"
start=$SECONDS
# Panel berhenti sebentar saat pengaturan disalin (data sapo-api utuh).
for _ in $(seq 1 300); do
  st="$(api GET /api/v1/system/install || true)"
  json 'import sys; sys.exit(0 if not v["running"] and (v.get("last") or {}).get("finished_at") else 1)' <<<"$st" 2>/dev/null && break
  sleep 4
done
json 'assert v["last"]["ok"], v' <<<"$st" || fail "pemasangan ke disk: $st"
echo "installer: Sapo terpasang di $sysdisk dalam $((SECONDS - start)) detik"

# --- Menu darurat di konsol (port serial) ----------------------------------------
# console_chat POLA KIRIM [POLA KIRIM ...]: tunggu POLA (regex) di keluaran
# serial lalu kirim teks (\n = Enter). Mencetak grup pertama dari pola
# terakhir yang cocok dan memuatnya.
console_chat() {
  python3 - "$WORK/serial.sock" "$@" <<'PY'
import re, socket, sys, time
sock = socket.socket(socket.AF_UNIX); sock.connect(sys.argv[1]); sock.settimeout(1)
steps, buf, found = sys.argv[2:], "", ""
for pat, send in zip(steps[0::2], steps[1::2]):
    deadline = time.time() + 180
    while not (m := re.search(pat, buf)):
        if time.time() > deadline:
            sys.stderr.write("tidak muncul: %r\n--- keluaran ---\n%s\n" % (pat, buf[-3000:])); sys.exit(1)
        try:
            buf += sock.recv(4096).decode("utf-8", "replace")
        except socket.timeout:
            pass
    found = m.group(1) if m.groups() else found
    buf = buf[m.end():]
    if send:
        sock.sendall(send.encode().decode("unicode_escape").encode())
print(found)
PY
}
console_chat '' '\n' '\(1-5\): ' '' >/dev/null || fail "menu darurat tidak tampil di konsol serial"
temp="$(console_chat '' '\n' '\(1-5\): ' '1\n' 'penguji untuk melanjutkan: ' 'penguji\n' \
  '\n    ([a-z2-9]{4}(?:-[a-z2-9]{4}){3})\r?\n' '')" || fail "reset password admin lewat menu darurat"
wait_panel 120 || fail "panel tidak kembali setelah reset password admin"
old_jar="$JAR"
res="$(api GET /api/v1/storage/pools)"
[[ "$res" == *' 401' ]] || fail "sesi lama harus berakhir setelah reset admin: $res"
JAR="$WORK/cookies-baru"
res="$(api POST /api/v1/auth/login "{\"username\":\"penguji\",\"password\":\"$PASS\",\"totp_code\":\"$(totp "$secret")\"}")"
[[ "$res" != *' 200' ]] || fail "password lama harus ditolak setelah reset: $res"
res="$(api POST /api/v1/auth/login "{\"username\":\"penguji\",\"password\":\"$temp\"}")"
[[ "$res" == *' 200' ]] || fail "login dengan password sementara (tanpa 2FA): $res"
status="$(api GET /api/v1/auth/status)"
json 'p=v["user"]["pending"]; assert "password" in p and "totp" in p, p' <<<"$status" || fail "admin harus wajib ganti password dan pasang 2FA: $status"
res="$(api GET /api/v1/storage/pools)"
[[ "$res" == *' 403'* || "$res" == *' 401' ]] || fail "panel harus terkunci sampai langkah wajib selesai: $res"
echo "menu darurat: password admin di-reset (sesi lama berakhir, password lama ditolak, wajib ganti password dan 2FA)"

console_chat '' '\n' '\(1-5\): ' '2\n' 'RESET[^\n]*: ' 'RESET\n' '(sudah di-reset)' '' >/dev/null || fail "reset jaringan lewat menu darurat"
start=$SECONDS
wait_panel 120 || fail "panel tidak kembali setelah reset jaringan"
echo "menu darurat: jaringan di-reset (DHCP), panel kembali dalam $((SECONDS - start)) detik"
JAR="$old_jar"

# --- Boot dari disk terpasang (tanpa ISO) -------------------------------------------
# Pengaturan yang tersalin adalah keadaan saat pemasangan (sebelum reset
# password admin di menu darurat).
kill "$qemu" 2>/dev/null || true
wait "$qemu" 2>/dev/null || true
disks2=()
for i in 1 2 3; do
  disks2+=(-drive "file=$WORK/disk$i.img,if=none,format=raw,id=d$i" -device "virtio-blk-pci,drive=d$i,serial=SAPOUJI$i")
done
disks2+=(-drive "file=$WORK/disk4.img,if=none,format=raw,id=d4" -device "virtio-blk-pci,drive=d4,serial=SAPOUJI4,bootindex=0")
qemu-system-x86_64 "${accel[@]}" -m "$VM_RAM_MB" -smp 2 "${disks2[@]}" \
  -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$PORT-:5000,hostfwd=tcp:127.0.0.1:$TLS_PORT-:5001,hostfwd=tcp:127.0.0.1:$SMB_PORT-:445" \
  -display none \
  -chardev "socket,id=ser1,path=$WORK/serial2.sock,server=on,wait=off,logfile=$LOG.disk" -serial chardev:ser1 &
qemu=$!
deadline=$((SECONDS + TIMEOUT_S))
until curl -kfsS --max-time 5 "$HTTPS/api/v1/health" 2>/dev/null | grep -q '"agent":true'; do
  kill -0 "$qemu" 2>/dev/null || fail "QEMU (boot dari disk) berhenti sebelum panel siap; lihat $LOG.disk"
  (( SECONDS <= deadline )) || fail "panel dari disk terpasang tidak siap dalam ${TIMEOUT_S} detik; lihat $LOG.disk"
  sleep 5
done
# Nama host dari sesi live (sapo-uji, diganti di uji jaringan) ikut
# terbawa, dan sertifikat panel memuatnya (dibuat ulang bila sertifikat
# lama belum memuat nama itu).
cert2="$(openssl s_client -connect "127.0.0.1:$TLS_PORT" -servername localhost </dev/null 2>/dev/null | openssl x509 -noout -text)"
[[ "$cert2" == *"DNS:sapo-uji"* ]] || fail "sertifikat sistem terpasang tidak memuat nama host sapo-uji: $(grep -A1 'Alternative Name' <<<"$cert2")"
JAR="$WORK/cookies-disk"
sleep 31 # kode 2FA berikutnya (kode yang sama tidak bisa dipakai ulang)
res="$(api POST /api/v1/auth/login "{\"username\":\"penguji\",\"password\":\"$PASS\",\"totp_code\":\"$(totp "$secret")\"}")"
[[ "$res" == *' 200' ]] || fail "login di sistem terpasang: $res"
res="$(api GET /api/v1/system/install)"
json 'assert not v["live"], v' <<<"$res" || fail "sistem terpasang tidak boleh dianggap ISO: $res"
for _ in $(seq 1 30); do
  pools="$(api GET /api/v1/storage/pools)"
  json 'import sys; sys.exit(0 if any(p["name"]=="tetap" and p["health"]=="ONLINE" for p in v["pools"]) else 1)' <<<"$pools" && break
  sleep 3
done
json 'assert any(p["name"]=="tetap" and p["health"]=="ONLINE" for p in v["pools"]), v' <<<"$pools" || fail "pool tetap tidak diimpor saat boot: $pools"
rm -f "$WORK/salinan.txt"
for _ in $(seq 1 20); do
  smbclient //127.0.0.1/tetap -p "$SMB_PORT" -m SMB3 -U "budi%$UPASS" -c "get catatan.txt $WORK/salinan.txt" >/dev/null 2>&1 && break
  sleep 3
done
cmp -s "$WORK/tetap.txt" "$WORK/salinan.txt" || fail "file di folder tetap tidak terbaca lewat SMB setelah boot dari disk"
echo "boot dari disk: panel menjawab, sertifikat memuat nama host sapo-uji, akun admin dan 2FA, pool tetap, dan SMB budi sama dengan sebelum dipasang"
echo "Uji asap lulus."
