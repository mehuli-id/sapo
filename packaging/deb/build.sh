#!/usr/bin/env bash
# Membangun paket sapo_<versi>_<arsitektur>.deb dari hasil build di repo.
# Pemakaian (dari root repo):  packaging/deb/build.sh   atau   make deb
# Variabel: ARCH (amd64|arm64, default: arsitektur mesin), VERSION, OUT_DIR.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
HERE="$ROOT/packaging/deb"
# shellcheck source=/dev/null
source "$HERE/package.conf"

ARCH="${ARCH:-$(dpkg --print-architecture 2>/dev/null || go env GOARCH)}"
case "$ARCH" in
  amd64|arm64) GOARCH="$ARCH" ;;
  *) echo "Arsitektur belum didukung: $ARCH" >&2; exit 1 ;;
esac

# Versi Debian dari git: tag v1.2.3 → 1.2.3; tanpa tag → 0.0.0~git<tanggal>.<sha>
if [[ -z "${VERSION:-}" ]]; then
  if tag=$(git -C "$ROOT" describe --tags --exact-match 2>/dev/null); then
    VERSION="${tag#v}"
  else
    sha=$(git -C "$ROOT" rev-parse --short=7 HEAD 2>/dev/null || echo unknown)
    date=$(git -C "$ROOT" log -1 --format=%cd --date=format:%Y%m%d 2>/dev/null || date +%Y%m%d)
    VERSION="0.0.0~git${date}.${sha}"
  fi
fi

# Build yang bisa diulang (reproducible): semua waktu file = waktu commit.
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || date +%s)}"
export SOURCE_DATE_EPOCH

OUT_DIR="${OUT_DIR:-$ROOT/dist}"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
PKG="$STAGE/sapo"

echo "Membangun sapo $VERSION ($ARCH)…"

# 1. Binary Go (statis, tanpa cgo).
mkdir -p "$PKG$PREFIX_BIN"
for cmd in sapod sapo-api; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go -C "$ROOT" build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o "$PKG$PREFIX_BIN/$cmd" "./cmd/$cmd"
done

# 2. Panel web (harus sudah dibangun: make web).
if [[ ! -f "$ROOT/web/dist/index.html" ]]; then
  echo "Panel belum dibangun. Jalankan dulu: make web" >&2
  exit 1
fi
mkdir -p "$PKG$PREFIX_SHARE"
cp -r "$ROOT/web/dist" "$PKG$PREFIX_SHARE/web"

# 3. Data: manifest fitur, wizard, branding, konfigurasi default.
cp -r "$ROOT/features" "$PKG$PREFIX_SHARE/features"
rm -f "$PKG$PREFIX_SHARE/features/README.md"
cp -r "$ROOT/wizards" "$PKG$PREFIX_SHARE/wizards"
cp "$ROOT/branding.yaml" "$PKG$PREFIX_SHARE/branding.yaml"
# Nilai bawaan ikut diperbarui paket; /etc/sapo/sapo.yaml (perubahan admin
# atau panel) dibuat oleh postinst dan tidak pernah ditimpa upgrade.
install -D -m 0644 "$ROOT/config/sapo.yaml" "$PKG$PREFIX_SHARE/sapo.yaml"

# 4. systemd & user sistem.
install -D -m 0644 "$HERE/systemd/sapod.service" "$PKG$UNIT_DIR/sapod.service"
install -D -m 0644 "$HERE/systemd/sapo-api.service" "$PKG$UNIT_DIR/sapo-api.service"
install -D -m 0644 "$HERE/sysusers.conf" "$PKG$SYSUSERS_DIR/sapo.conf"
# Layar login konsol menampilkan alamat panel dan sidik jari sertifikat
# HTTPS. File-nya ditulis sapod (root) dari templat tetap, atas permintaan
# sapo-api (operasi console.panel_address, agent.console.issue_file).
install -d -m 0755 "$PKG/etc/issue.d"
ln -s "$CONSOLE_ISSUE" "$PKG/etc/issue.d/sapo.issue"

# 5. Dokumentasi & lisensi.
install -D -m 0644 "$HERE/copyright" "$PKG/usr/share/doc/sapo/copyright"
install -D -m 0644 "$ROOT/README.md" "$PKG/usr/share/doc/sapo/README.md"
install -D -m 0644 "$ROOT/TRADEMARKS.md" "$PKG/usr/share/doc/sapo/TRADEMARKS.md"
{
  echo "sapo ($VERSION) unstable; urgency=medium"
  echo ""
  echo "  * Build otomatis dari commit $(git -C "$ROOT" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)."
  echo "    Riwayat lengkap: https://github.com/mehuli-id/sapo/commits"
  echo ""
  echo " -- PT. Mehuli Data Media <$MAINTAINER_EMAIL>  $(date -u -d "@$SOURCE_DATE_EPOCH" -R)"
} | gzip -9n > "$PKG/usr/share/doc/sapo/changelog.Debian.gz"

# Izin standar: direktori 0755, file 0644, binary 0755.
find "$PKG" -type d -exec chmod 0755 {} +
find "$PKG" -type f -exec chmod 0644 {} +
chmod 0755 "$PKG$PREFIX_BIN"/*

# 6. Metadata paket. Depends diambil dari manifest fitur "always" (data).
DEPENDS="$(go -C "$ROOT" run ./tools/featuredeps -dir "$ROOT/features" -activation always)"
mkdir -p "$PKG/DEBIAN"
INSTALLED_SIZE=$(du -sk --apparent-size --exclude=DEBIAN "$PKG" | cut -f1)
sed -e "s|@VERSION@|$VERSION|" -e "s|@ARCH@|$ARCH|" -e "s|@DEPENDS@|$DEPENDS|" \
    -e "s|@INSTALLED_SIZE@|$INSTALLED_SIZE|" -e "s|@MAINTAINER_EMAIL@|$MAINTAINER_EMAIL|" \
    "$HERE/control.in" > "$PKG/DEBIAN/control"
for s in postinst prerm postrm; do install -m 0755 "$HERE/$s" "$PKG/DEBIAN/$s"; done
(cd "$PKG" && find . -path ./DEBIAN -prune -o -type f -print0 | sort -z | xargs -0 md5sum | sed 's| \./| |') > "$PKG/DEBIAN/md5sums"

find "$PKG" -exec touch --no-dereference --date="@$SOURCE_DATE_EPOCH" {} +

mkdir -p "$OUT_DIR"
OUT="$OUT_DIR/sapo_${VERSION}_${ARCH}.deb"
dpkg-deb --root-owner-group -Zxz --build "$PKG" "$OUT" >/dev/null
echo "Selesai: $OUT"
