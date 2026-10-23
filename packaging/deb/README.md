# Paket .deb

Paket `sapo` untuk Debian stable (Fase 1: untuk penguji dan kontributor). Dipasang di atas Debian minimal; paket lain dipasang sesuai fitur yang diaktifkan.

## Membangun

```bash
make deb                 # arsitektur mesin ini
make deb ARCH=arm64      # cross-build untuk arm64
```

Hasilnya di `dist/sapo_<versi>_<arsitektur>.deb`. Versi diambil dari git: tag `v1.2.3` menjadi `1.2.3`, selain itu `0.0.0~git<tanggal>.<sha>`. CI GitHub Actions membangun paket amd64 dan arm64 di setiap push sebagai artifact.

## Memasang (di VM uji)

```bash
sudo apt install ./sapo_<versi>_amd64.deb
```

Lalu buka `http://<nama-host>.local:5000` atau `http://<IP-NAS>:5000` dari komputer di jaringan yang sama. Halaman itu menampilkan sidik jari sertifikat dan mengarahkan ke panel HTTPS di port 5001 (ADR 0004). Setelah itu ikuti wizard setup awal.

## Isi paket

| Lokasi di sistem | Sumber di repo |
|---|---|
| `/usr/lib/sapo/sapod`, `/usr/lib/sapo/sapo-api` | `cmd/` (dibangun statis) |
| `/usr/share/sapo/web/` | `web/dist/` (hasil `make web`) |
| `/usr/share/sapo/features/` | `features/` |
| `/usr/share/sapo/wizards/` | `wizards/` |
| `/usr/share/sapo/branding.yaml` | `branding.yaml` |
| `/usr/share/sapo/sapo.yaml` (nilai bawaan, ikut diperbarui) | `config/sapo.yaml` |
| `/etc/sapo/sapo.yaml` (perubahan admin/panel, dibuat postinst) | — |
| `/usr/lib/systemd/system/sapod.service`, `sapo-api.service` | `packaging/deb/systemd/` |
| `/usr/lib/sysusers.d/sapo.conf` | `packaging/deb/sysusers.conf` |
| `/etc/issue.d/sapo.issue` (tautan ke `/run/sapo-api/console.issue`: alamat panel dan sidik jari sertifikat di layar login konsol) | ditulis `sapo-api` saat berjalan |

Baris `Depends` tidak ditulis tangan: diambil dari manifest fitur dengan `activation: always` (`go run ./tools/featuredeps`). Paket untuk fitur lain (Samba, ZFS, Docker, VPN) dipasang oleh `sapod` saat fitur diaktifkan dari panel.

Konfigurasi berlapis: nilai bawaan program → `/usr/share/sapo/sapo.yaml` → `/etc/sapo/sapo.yaml`. File di `/etc` hanya berisi nilai yang diubah admin atau panel, jadi upgrade tidak pernah menimpanya dan tidak pernah memunculkan pertanyaan dpkg (aman untuk upgrade otomatis).

Paket bisa dibangun ulang dengan hasil identik (reproducible build): waktu file diambil dari commit (`SOURCE_DATE_EPOCH`).

Nilai yang bisa berubah (alamat maintainer, lokasi pemasangan) ada di `package.conf`. **Ganti `MAINTAINER_EMAIL` dengan alamat resmi sebelum rilis publik.**

## Perilaku instalasi

| Kejadian | Yang dilakukan |
|---|---|
| Pasang pertama | Membuat user sistem `sapo` (tanpa login), `/var/lib/sapo` (0750), `/etc/sapo/sapo.yaml` kosong, lalu mengaktifkan dan menyalakan layanan (juga berjalan di chroot, misalnya saat membangun ISO) |
| Upgrade | Mempertahankan `/etc/sapo/sapo.yaml`, data akun, dan status layanan (layanan yang dinonaktifkan admin tetap nonaktif); layanan di-restart. Gagal start tidak menggagalkan dpkg |
| `apt remove` | Menghentikan layanan; konfigurasi dan data akun tetap ada. Bila dipasang ulang, layanan aktif kembali |
| `apt purge` | Menghapus konfigurasi dan data panel (`/var/lib/sapo`: akun & log audit). User sistem `sapo` dibiarkan sesuai Debian Policy |

**Data pengguna (pool, shared folder, Sapo Drive) tidak pernah disentuh oleh paket ini**, termasuk saat purge.

Unit systemd sudah dikeraskan: `sapo-api` berjalan sebagai user `sapo` tanpa capability apa pun, dan `sapod` tidak punya akses jaringan selain unix socket.
