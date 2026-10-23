# ADR 0001: Debian stable minimal sebagai base OS

- **Status:** Diterima
- **Tanggal:** 2026-10-23

## Konteks

Sapo membutuhkan base OS untuk NAS yang menyimpan data penting dan dipakai bertahun-tahun. Kebutuhan: stabil, dukungan panjang, paket storage lengkap (Samba, NFS, mdadm, Btrfs, OpenZFS, Docker, WireGuard), mudah dipaketkan ke `.deb` dan ISO, serta tidak bergantung pada kebijakan satu perusahaan.

## Keputusan

Sapo memakai **Debian stable minimal** (tanpa desktop dan tanpa "standard system utilities"). Hanya paket inti yang selalu terpasang; paket untuk setiap fitur dipasang saat fitur itu diaktifkan dari panel, berdasarkan manifest fitur di `features/` (data, bukan kode).

OpenZFS dipasang lewat `zfs-dkms` (modul dikompilasi di mesin pengguna), mengikuti praktik Debian untuk lisensi CDDL.

## Alasan

- Stabil dan konservatif; sekitar 5 tahun dukungan per versi (termasuk LTS).
- Dikelola komunitas, bukan perusahaan.
- Paket NAS lengkap, termasuk `zfs-dkms`.
- Sudah terbukti menjadi dasar TrueNAS Community Edition dan OpenMediaVault.
- Tooling resmi untuk paket `.deb`, repo APT, dan ISO (`live-build`).

## Alternatif yang dipertimbangkan

| OS | Alasan tidak dipilih |
|---|---|
| Ubuntu | Dikendalikan satu perusahaan; arah Snap/Ubuntu Pro bisa berubah |
| Alpine | musl; tool storage dan ZFS kurang teruji |
| Fedora | Siklus rilis terlalu pendek untuk NAS |
| Rocky/Alma | ZFS dan Samba versi baru kurang mulus |
| Arch | Rolling release |
| NixOS | Kurva belajar tinggi untuk kontributor |
| FreeBSD | Tidak ada Docker, dukungan hardware lebih sempit |

## Konsekuensi

- Paket cenderung versi lama → kernel dari Debian backports untuk hardware baru; aplikasi terbaru lewat Docker.
- Rollback update disediakan Sapo sendiri lewat ZFS boot environments.
- Pendapat hukum soal distribusi OpenZFS diminta sebelum rilis 1.0.
