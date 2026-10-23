#!/usr/bin/env bash
# Menambahkan paket .deb ke repo APT Sapo, lalu membuat ulang indeks dan
# tanda tangan semua saluran.
# Pemakaian: packaging/apt/build-repo.sh <dir-repo> <saluran> [file.deb ...]
#   <dir-repo>  folder repo (isi cabang gh-pages/apt), dibuat bila belum ada
#   <saluran>   salah satu CHANNELS di repo.conf (misalnya stable atau dev)
# Kunci rahasia harus sudah ada di GNUPGHOME (sidik jari KEY_FINGERPRINT).
# Kebutuhan: dpkg-deb, apt-ftparchive (apt-utils), gpg, sha256sum, md5sum.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=/dev/null
source "$HERE/repo.conf"

REPO="${1:?Pemakaian: $0 <dir-repo> <saluran> [file.deb ...]}"
CHANNEL="${2:?saluran wajib diisi}"
shift 2
[[ " $CHANNELS " == *" $CHANNEL "* ]] || { echo "Saluran tidak dikenal: $CHANNEL (pilihan: $CHANNELS)" >&2; exit 1; }
gpg --list-secret-keys "$KEY_FINGERPRINT" >/dev/null 2>&1 || { echo "Kunci rahasia $KEY_FINGERPRINT tidak ada di GNUPGHOME." >&2; exit 1; }

POOL="pool/$COMPONENT/s/sapo"
mkdir -p "$REPO/$POOL" "$REPO/dists" "$REPO/channels"

# 1. Paket baru masuk pool dan dicatat di daftar saluran. Nama sama dengan
#    isi berbeda ditolak: versi yang sudah terbit tidak boleh berubah.
for deb in "$@"; do
  name="$(basename "$deb")"
  [[ "$(dpkg-deb -f "$deb" Package)" == sapo ]] || { echo "$name bukan paket sapo" >&2; exit 1; }
  if [[ -e "$REPO/$POOL/$name" ]]; then
    cmp -s "$deb" "$REPO/$POOL/$name" || { echo "$name sudah terbit dengan isi berbeda" >&2; exit 1; }
  else
    install -m 0644 "$deb" "$REPO/$POOL/$name"
  fi
  # Daftar paket per saluran ada di channels/ (di luar dists/, jadi tidak
  # ikut di Release).
  list="$REPO/channels/$CHANNEL.list"
  touch "$list"
  grep -qxF "$POOL/$name" "$list" || echo "$POOL/$name" >> "$list"
done

# 2. Simpan KEEP_VERSIONS versi terbaru per saluran (per arsitektur).
for ch in $CHANNELS; do
  list="$REPO/channels/$ch.list"
  touch "$list"
  for arch in $ARCHS; do
    { grep "_${arch}\.deb$" "$list" || true; } | while read -r f; do
      echo "$(dpkg-deb -f "$REPO/$f" Version) $f"
    done | sort -V -r -k1,1 | tail -n +"$((KEEP_VERSIONS + 1))" | cut -d' ' -f2
  done > "$list.drop"
  grep -vxF -f "$list.drop" "$list" > "$list.new" || true
  mv "$list.new" "$list"
  rm -f "$list.drop"
done
# File pool yang tidak dirujuk saluran mana pun dihapus.
for f in "$REPO/$POOL"/*.deb; do
  [[ -e "$f" ]] || continue
  rel="$POOL/$(basename "$f")"
  if ! cat "$REPO"/channels/*.list | grep -qxF "$rel"; then rm -f "$f"; fi
done

# 3. Indeks Packages per saluran dan arsitektur, lalu Release bertanda tangan.
for ch in $CHANNELS; do
  for arch in $ARCHS; do
    dir="$REPO/dists/$ch/$COMPONENT/binary-$arch"
    mkdir -p "$dir"
    : > "$dir/Packages"
    { grep "_${arch}\.deb$" "$REPO/channels/$ch.list" || true; } | sort | while read -r f; do
      deb="$REPO/$f"
      dpkg-deb -f "$deb"
      echo "Filename: $f"
      echo "Size: $(stat -c %s "$deb")"
      echo "MD5sum: $(md5sum "$deb" | cut -d' ' -f1)"
      echo "SHA256: $(sha256sum "$deb" | cut -d' ' -f1)"
      echo
    done >> "$dir/Packages"
    gzip -9nkf "$dir/Packages"
  done
  rm -f "$REPO/dists/$ch/Release" "$REPO/dists/$ch/InRelease" "$REPO/dists/$ch/Release.gpg"
  apt-ftparchive \
    -o "APT::FTPArchive::Release::Origin=$ORIGIN" \
    -o "APT::FTPArchive::Release::Label=$LABEL" \
    -o "APT::FTPArchive::Release::Suite=$ch" \
    -o "APT::FTPArchive::Release::Codename=$ch" \
    -o "APT::FTPArchive::Release::Architectures=$ARCHS" \
    -o "APT::FTPArchive::Release::Components=$COMPONENT" \
    -o "APT::FTPArchive::Release::Acquire-By-Hash=no" \
    release "$REPO/dists/$ch" > "$REPO/dists/$ch/Release.tmp"
  mv "$REPO/dists/$ch/Release.tmp" "$REPO/dists/$ch/Release"
  gpg --batch --yes --local-user "$KEY_FINGERPRINT" --clearsign -o "$REPO/dists/$ch/InRelease" "$REPO/dists/$ch/Release"
  gpg --batch --yes --local-user "$KEY_FINGERPRINT" --armor --detach-sign -o "$REPO/dists/$ch/Release.gpg" "$REPO/dists/$ch/Release"
done

# 4. Kunci publik dan petunjuk di akar repo.
install -m 0644 "$HERE/sapo-archive-key.asc" "$REPO/sapo-archive-key.asc"
cat > "$REPO/README.txt" <<TXT
Repo APT Sapo by Mehuli. NAS Sapo sudah memakainya otomatis (paket sapo
memasang $SOURCES dan $KEYRING).
Sidik jari kunci penanda tangan: $KEY_FINGERPRINT
Saluran: $CHANNELS
TXT
echo "Repo diperbarui: saluran $CHANNEL, $(wc -l < "$REPO/channels/$CHANNEL.list") paket."
