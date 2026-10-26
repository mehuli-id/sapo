#!/usr/bin/env bash
# Membangun ISO live Sapo dengan Debian live-build.
# Pemakaian (dari root repo, di Debian dengan paket live-build, sebagai root):
#   make iso   (atau: make deb ARCH=amd64 && sudo packaging/iso/build.sh)
# FLAVOR=release untuk ISO rilis (tanpa akun debug), bawaan dev.
# Paket sapo diambil dari dist/sapo_*_<ARCH>.deb (hasil make deb).
# Hasil: dist/<ISO_NAME>_<versi>_<ARCH>.iso beserta file .sha256.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
HERE="$ROOT/packaging/iso"
# shellcheck source=/dev/null
source "$HERE/iso.conf"
FLAVOR="${FLAVOR:-dev}"
case "$FLAVOR" in
  dev | release) ;;
  *) echo "FLAVOR harus dev atau release (bukan $FLAVOR)." >&2; exit 1 ;;
esac
# Nilai per jenis dari iso.conf (ISO_NAME_dev, ISO_NAME_release, …).
pick() { local v="$1_$FLAVOR"; printf '%s' "${!v}"; }
ISO_NAME="$(pick ISO_NAME)"
ISO_VOLUME="$(pick ISO_VOLUME)"
ISO_APPLICATION="$(pick ISO_APPLICATION)"
ISO_ISSUE="$(pick ISO_ISSUE)"
OUT_DIR="${OUT_DIR:-$ROOT/dist}"
WORK="$ROOT/$WORK_DIR"

if [[ $EUID -ne 0 ]]; then
  echo "live-build butuh root (chroot dan mount). Jalankan dengan sudo." >&2
  exit 1
fi
command -v lb >/dev/null || { echo "Paket live-build belum terpasang (apt install live-build)." >&2; exit 1; }

# Tepat satu paket sapo untuk arsitektur ini, supaya isi ISO tidak ambigu.
shopt -s nullglob
debs=("$OUT_DIR"/sapo_*_"$ARCH".deb)
shopt -u nullglob
if [[ ${#debs[@]} -ne 1 ]]; then
  echo "Harus ada tepat satu $OUT_DIR/sapo_*_$ARCH.deb (ada ${#debs[@]}). Jalankan: make deb ARCH=$ARCH" >&2
  exit 1
fi
DEB="${debs[0]}"
VERSION="$(dpkg-deb -f "$DEB" Version)"

# Waktu file di ISO = waktu commit, seperti paket .deb.
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || date +%s)}"
export SOURCE_DATE_EPOCH

echo "Membangun $ISO_NAME $VERSION ($FLAVOR, $ARCH, Debian $DISTRIBUTION)…"
rm -rf "$WORK"
mkdir -p "$WORK"
cd "$WORK"

lb config \
  --distribution "$DISTRIBUTION" \
  --architecture "$ARCH" \
  --archive-areas "$ARCHIVE_AREAS" \
  --mirror-bootstrap "$MIRROR" \
  --mirror-chroot "$MIRROR" \
  --mirror-binary "$MIRROR" \
  --binary-image iso-hybrid \
  --debian-installer none \
  --apt-recommends false \
  --firmware-chroot false \
  --firmware-binary false \
  --memtest none \
  --bootappend-live "$BOOT_APPEND" \
  --iso-volume "$ISO_VOLUME" \
  --iso-application "$ISO_APPLICATION" \
  --iso-publisher "$ISO_PUBLISHER"

# Menu boot dengan jeda tunggu (bawaan live-build: menunggu tanpa batas).
# Konfigurasi bawaan disalin lalu diubah; gagal keras bila formatnya berubah.
LB_BOOTLOADERS=/usr/share/live/build/bootloaders
mkdir -p config/bootloaders
cp -a "$LB_BOOTLOADERS/isolinux" "$LB_BOOTLOADERS/grub-pc" config/bootloaders/
sed -i "s/^timeout .*/timeout $((BOOT_TIMEOUT_S * 10))/" config/bootloaders/isolinux/isolinux.cfg
grep -qx "timeout $((BOOT_TIMEOUT_S * 10))" config/bootloaders/isolinux/isolinux.cfg \
  || { echo "Gagal mengatur jeda menu isolinux." >&2; exit 1; }
printf '\nset timeout=%s\n' "$BOOT_TIMEOUT_S" >> config/bootloaders/grub-pc/config.cfg

# Daftar paket dan file tambahan (data di packaging/iso/).
lists=("$HERE/packages.list")
if [[ "$FLAVOR" == dev ]]; then lists+=("$HERE/packages-dev.list"); fi
cat "${lists[@]}" | grep -v '^\s*#' | sed '/^\s*$/d' > config/package-lists/sapo.list.chroot
cp -r "$HERE/includes.chroot/." config/includes.chroot/
# Teks di layar login konsol (dibuang installer, agent.install.remove_paths).
mkdir -p config/includes.chroot/etc/issue.d
printf '%s\n' "$ISO_ISSUE" > config/includes.chroot/etc/issue.d/sapo-iso.issue
# Pemeriksaan di dalam chroot (misalnya modul ZFS terkompilasi).
cp "$HERE"/hooks/*.hook.chroot config/hooks/normal/
# Paket lokal: live-build membuat repo APT sementara darinya, jadi Depends
# paket sapo dipasang oleh apt seperti biasa.
cp "$DEB" config/packages.chroot/

lb build

mkdir -p "$OUT_DIR"
OUT="$OUT_DIR/${ISO_NAME}_${VERSION}_${ARCH}.iso"
mv "live-image-$ARCH.hybrid.iso" "$OUT"
(cd "$OUT_DIR" && sha256sum "$(basename "$OUT")" > "$(basename "$OUT").sha256")
echo "Selesai: $OUT"
