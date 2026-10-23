# Repo APT

Repo APT resmi Sapo, bertanda tangan, di-hosting GitHub Pages: **https://mehuli-id.github.io/sapo/apt**. NAS Sapo memakainya otomatis: paket `sapo` memasang

- kunci publik `/usr/share/keyrings/sapo-archive-keyring.gpg`;
- sumber APT `/etc/apt/sources.list.d/sapo.sources`, saluran `stable`.

Jadi `apt update && apt upgrade` di NAS memperbarui Sapo dengan paket yang tanda tangannya diperiksa.

## Saluran

| Saluran | Isi | Cara terbit |
|---|---|---|
| `stable` | Rilis resmi | Push tag `v*` (versi berbasis tanggal, misalnya `v26.12`). Paket juga dilampirkan di GitHub Release. |
| `dev` | Build dari cabang mana pun, untuk penguji | Tab *Actions* → **Rilis** → *Run workflow* |

Setiap saluran menyimpan `KEEP_VERSIONS` versi terakhir per arsitektur (amd64, arm64). Semua saluran selalu diterbitkan dan ditandatangani walau kosong, supaya `apt update` di NAS tidak pernah gagal.

Untuk pindah saluran, ubah `Suites:` di `/etc/apt/sources.list.d/sapo.sources`. File itu adalah conffile, jadi upgrade tidak menimpa perubahan admin. Pengaturan ini nantinya tersedia di panel (fitur Update).

Setelah terbit, workflow memasang paket lewat `apt` di container Debian bersih. Peringatan apa pun dari `apt`, termasuk tanda tangan, membuat workflow gagal.

## Kunci penanda tangan

- **Jenis:** ed25519, khusus tanda tangan, berlaku sampai 22 Oktober 2029.
- **Sidik jari:** `2379 7621 6EBA 1A73 6DF6  C54D 40BB EB6F C1DC 81B4`
- **Kunci publik:** `sapo-archive-key.asc` di folder ini. Juga tersedia di akar repo APT.
- **Kunci rahasia:** secret `APT_SIGNING_KEY` di GitHub. Kunci ini hanya dimuat ke GNUPGHOME sementara selama langkah tanda tangan. Cadangan terenkripsi dan sertifikat pencabutannya disimpan pemilik proyek di luar repo.

### Memperpanjang atau mengganti kunci

1. **Memperpanjang** (sidik jari tetap):

   ```bash
   gpg --quick-set-expire <sidik-jari> 3y
   ```

   Lalu perbarui `APT_SIGNING_KEY` dan `sapo-archive-key.asc`.
2. **Mengganti kunci:**
   1. Terbitkan dulu versi `sapo` yang membawa kunci lama dan kunci baru di keyring yang sama.
   2. Setelah NAS memperbaruinya, mulai tandatangani repo dengan kunci baru.
   3. Lepas kunci lama di rilis berikutnya.

   Bila langsung diganti, NAS yang belum punya kunci baru tidak bisa update lagi.
3. **Kunci bocor:**
   1. Terbitkan sertifikat pencabutan.
   2. Lakukan langkah 2 secepatnya.
   3. Umumkan lewat `SECURITY.md`.

## Isi folder

| File | Isi |
|---|---|
| `repo.conf` | URL, saluran, komponen, arsitektur, jumlah versi yang disimpan, sidik jari, lokasi kunci dan sumber APT di NAS |
| `build-repo.sh` | Menambah paket ke pool, menyusun indeks `Packages`, lalu membuat `Release`, `InRelease`, dan `Release.gpg` untuk semua saluran. Paket dengan nama sama tetapi isi berbeda ditolak. |
| `sapo-archive-key.asc` | Kunci publik penanda tangan |

Workflow: `.github/workflows/release.yml`. Isi cabang `gh-pages` dikelola workflow, jadi jangan diubah manual.
