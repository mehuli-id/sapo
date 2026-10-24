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
# Kebutuhan: qemu-system-x86_64, curl, openssl, python3, mkfs.ext4, smbclient,
# klien NFS (mount.nfs4) dan sudo tanpa password. KVM dipakai bila tersedia.
set -euo pipefail

ISO="${1:?Pemakaian: $0 <file.iso>}"
PORT="${PORT:-15000}"              # port HTTP di host, diteruskan ke 5000 di VM
TLS_PORT="${TLS_PORT:-15001}"      # port HTTPS di host, diteruskan ke 5001 di VM
SMB_PORT="${SMB_PORT:-14450}"      # port SMB di host, diteruskan ke 445 di VM
NFS_PORT="${NFS_PORT:-12049}"      # port NFS di host, diteruskan ke 2049 di VM
RPCBIND_PORT="${RPCBIND_PORT:-10111}" # diteruskan ke 111 (rpcbind) di VM, harus tertutup
MOUNTD_PORT="${MOUNTD_PORT:-20048}"   # diteruskan ke 20048 (mountd) di VM, harus tertutup
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
  -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$PORT-:5000,hostfwd=tcp:127.0.0.1:$TLS_PORT-:5001,hostfwd=tcp:127.0.0.1:$SMB_PORT-:445,hostfwd=tcp:127.0.0.1:$NFS_PORT-:2049,hostfwd=tcp:127.0.0.1:$RPCBIND_PORT-:111,hostfwd=tcp:127.0.0.1:$MOUNTD_PORT-:20048" \
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
echo "Uji asap lulus."
