# ADR 0009: Pengaturan jaringan dari panel

- **Status:** Diterima
- **Tanggal:** 2026-10-24

## Konteks

NAS biasanya butuh alamat tetap dan nama host yang mudah diingat. Sampai sekarang jaringan memakai bawaan Debian (ifupdown, DHCP), dan mengubahnya butuh terminal (melanggar prinsip 2). Salah mengatur jaringan dari panel bisa memutus NAS dari semua komputer, termasuk panel itu sendiri.

## Keputusan

- **Aplikasi Jaringan** (admin): nama host, mode setiap antarmuka fisik (DHCP, alamat IPv4 tetap dengan prefiks dan gateway, atau tidak dipakai), dan server DNS. IPv6 selalu otomatis (SLAAC). Antarmuka yang tidak dipakai tidak dinyalakan (port tanpa kabel tidak menunda boot menunggu DHCP); minimal satu antarmuka harus dipakai.
- **`/etc/network/interfaces` ditulis utuh oleh `sapod`** dari templat tetap (`network.apply`); `/etc/resolv.conf` hanya bila semua antarmuka beralamat tetap (dengan DHCP, DNS dari router), `/etc/hosts` (baris 127.0.1.1) dan `hostnamectl` untuk nama host. Hanya file yang memang berubah yang disimpan dan dikembalikan, dan file berupa symlink (dikelola program lain, misalnya systemd-resolved) tidak pernah ditimpa. Semua nilai divalidasi `sapod`: hanya antarmuka fisik yang ada, alamat IPv4 /8–/30 yang bukan alamat jaringan atau broadcast, gateway di jaringan yang sama, paling banyak satu gateway, 1–3 server DNS unicast.
- **Pengaturan yang bukan buatan Sapo tidak hilang diam-diam**: bila `/etc/network/interfaces` belum dikelola Sapo dan berisi selain pengaturan dasar (loopback, DHCP, SLAAC, manual) — misalnya bond, bridge, VLAN, alamat tetap, perintah hook, atau file di `interfaces.d` — panel menolak dan meminta reset dari menu darurat dulu. Pengaturan dasar yang diganti disimpan sebagai `interfaces.before-sapo-<waktu>` di direktori `sapod`.
- **Setiap perubahan harus dikonfirmasi**:
  - `network.apply` menyimpan salinan semua file lama (0600, `agent.network.pending_file`), mengembalikan token acak 256 bit, lalu menerapkan pengaturan baru setelah `apply_delay_seconds` (agar jawaban sampai ke browser sebelum alamat berubah).
  - Panel mengonfirmasi sendiri bila masih bisa dibuka di alamat yang sama; bila alamat berubah, admin membuka tautan ke alamat baru (`#network-confirm=<token>`, token di bagian `#` sehingga tidak tercatat di log). `POST /api/v1/network/confirm` dan `/cancel` hanya butuh token, karena admin belum login di alamat baru.
  - Tanpa konfirmasi dalam `confirm_seconds` (bawaan 120, dihitung sejak pengaturan baru selesai dipasang), pengaturan lama dikembalikan, dan jaringan dicoba dinyalakan lagi sampai tiga kali. Juga bila `sapod` atau NAS berhenti sebelum konfirmasi: saat `sapod` menyala, salinan lama dikembalikan sebelum `sapod` menerima operasi. File ditulis dengan fsync (file dan direktori), agar listrik mati tidak meninggalkan file kosong.
  - Antarmuka yang gagal naik tidak dianggap gagal (konfirmasi yang menentukan); hanya gagal menulis file yang langsung dikembalikan.
  - Hanya satu perubahan menunggu pada satu waktu.
- **Menerapkan butuh password admin** dan tercatat di log audit, beserta konfirmasi dan pembatalan yang berhasil (yang gagal tidak dicatat, agar token salah tanpa login tidak membanjiri log).
- **Status menampilkan pengaturan yang dikelola Sapo** hanya bila `/etc/network/interfaces` masih persis yang ditulis `sapod` (hash di `state_file`). Setelah reset dari menu darurat (ADR 0008) atau diubah tangan, panel menampilkan "belum diatur dari panel".
- Nama host mesin dibaca `sapo-api` dari `/etc/hostname` (di-cache beberapa detik; prosesnya berjalan dengan `ProtectHostname`), jadi nama baru dan `<nama>.local` langsung lolos validasi Host.
- **Reset jaringan dari menu darurat** menghentikan `sapod` dan membuang perubahan yang menunggu konfirmasi, agar pengembalian otomatis tidak menimpa hasil reset.

## Alasan

- Konfirmasi dengan pengembalian otomatis (seperti pengaturan layar di desktop atau "commit confirmed" di router) membuat salah ketik alamat tidak pernah mengunci NAS lebih dari beberapa menit. Menu darurat tetap ada untuk keadaan lain.
- File ditulis utuh dari data yang diperiksa, sama dengan `smb.conf` dan ekspor NFS: tidak ada teks bebas dari panel yang masuk ke konfigurasi sistem.

## Konsekuensi

- Pengaturan lanjutan (bonding, VLAN, bridge, IPv6 tetap, Wi-Fi) belum ada; antarmuka virtual tidak disentuh.
- Sertifikat HTTPS tetap memuat nama host lama sampai `sapo-api` dinyalakan ulang (lalu dibuat ulang dengan sidik jari baru di layar konsol). Sertifikat self-signed selalu diperingatkan browser, jadi panel tetap bisa dibuka; sidik jari tetap cara memeriksanya.
