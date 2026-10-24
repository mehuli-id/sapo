# ADR 0008: Menu darurat di konsol NAS

- **Status:** Diterima
- **Tanggal:** 2026-10-24

## Konteks

Bila password admin dan kode cadangan 2FA hilang, atau pengaturan jaringan dan akses panel membuat panel tidak bisa dibuka, pemilik NAS butuh jalan pulih tanpa terminal Linux (prinsip 2) dan tanpa menghapus data (prinsip 1). NAS lain memakai tombol reset fisik; PC rakitan tidak punya.

## Keputusan

- **`sapo-console`**, program terpisah yang dijalankan systemd sebagai root di terminal fisik (`sapo-console@tty1.service`), menggantikan layar login tty1 (`getty@tty1` di-mask sekali oleh postinst; saat upgrade baru berlaku setelah boot berikutnya, agar sesi apt di tty1 tidak terputus; dikembalikan saat paket dihapus). Login konsol tetap ada di tty2 dan seterusnya. ISO dev juga memasangnya di port serial (`ttyS0`) untuk uji asap; image produksi tidak.
  - Unit dibatasi (`ProtectSystem=strict`, hanya `/etc/network`, `/etc/sapo`, `/var/lib/sapo` yang bisa ditulis) dan selalu dinyalakan lagi tanpa batas start.
  - Konfigurasi admin yang rusak tidak mematikan menu: menu memakai nilai bawaan dan menampilkan peringatan.
- **Siapa pun yang memegang keyboard dan layar NAS dianggap pemilik**, seperti tombol reset. Menu tidak menerima masukan dari jaringan; tidak ada operasi `sapod` atau API baru, jadi `sapo-api` yang disusupi tidak bisa memakainya. Port serial, KVM-over-IP, dan konsol VM setara konsol fisik dan harus dilindungi pemiliknya.
- Menu menampilkan alamat panel dan sidik jari sertifikat (dari file layar login yang ditulis `sapod`, ADR 0004), digambar ulang setiap 30 detik. Teks dari file dibersihkan dari karakter kendali.
- **Data panel dibuka lewat proses pembantu** (`sapo-console helper`) yang berjalan sebagai pemilik `api.data_dir` (user `sapo`) tanpa grup tambahan. Direktori itu milik `sapo-api`, yang bisa saja disusupi dan menanam symlink atau hardlink; root tidak pernah membuka file di dalamnya. Nama admin dari data itu hanya ditampilkan bila sesuai `auth.username_pattern`.
- **Reset password admin**: admin dipilih (bila lebih dari satu) dan nama penggunanya diketik ulang. Panel dihentikan dulu (data akun disimpan di memorinya dan semua sesi berakhir), lalu: password diganti password sementara acak (16 huruf/angka, sekitar 80 bit, hanya tampil sekali di layar dan dihapus dari layar setelah 10 menit), wajib diganti saat login, 2FA dan kode cadangan dihapus (didaftarkan ulang saat login, `auth.require_totp_for_admin`), akun diaktifkan kembali. Pengguna lain dan data tidak berubah. Password Samba admin itu baru berganti setelah password panel diganti (layar menyebutkannya). Hanya akun administrator yang bisa di-reset.
- **Reset jaringan**: dengan mengetik `RESET`. Jaringan dihentikan dengan pengaturan lama, `/etc/network/interfaces` ditulis ulang (loopback, DHCP IPv4 dan SLAAC IPv6 di setiap antarmuka fisik, tanpa `interfaces.d`), dan kunci `api.listen`, `api.allowed_networks`, `api.denied_networks`, `api.hostnames`, `api.tls` dihapus dari `/etc/sapo/sapo.yaml` (kembali ke bawaan paket; isi lain dan komentarnya tetap). File yang tidak bisa diubah dengan aman (YAML rusak, alias, beberapa dokumen) dipindah utuh ke cadangan. File lama disimpan di sampingnya (`.sapo-<waktu>`, tidak pernah menimpa cadangan lain) dengan mode dan pemilik yang sama. Lalu jaringan dan panel dinyalakan lagi.
- **Restart dan matikan** dengan konfirmasi `y`.
- Semua aksi dicatat di log audit panel dengan pelaku `console`. Gagal menulis log audit tidak membatalkan pemulihan.
- Daftar layanan, file, kunci yang di-reset, dan panjang password sementara ada di `recovery` di `sapo.yaml`. Teks menu ada di `internal/console/messages.json` (Indonesia dan Inggris, mengikuti bahasa setup awal).

## Alasan

- Konsol fisik adalah satu-satunya jalur yang tidak bergantung pada jaringan atau password, dan sudah dipakai untuk menampilkan alamat panel.
- Program terpisah yang hanya membaca keyboard menjaga ADR 0002: tidak ada jalur dari jaringan ke aksi ini.

## Alternatif

- **Reset lewat file di USB** (misalnya `sapo-reset.txt`): tidak butuh monitor, tetapi siapa pun bisa mencolokkan USB tanpa terlihat, dan lebih sulit dijelaskan. Bisa ditambahkan nanti untuk NAS tanpa monitor.
- **Mode pemulihan di menu boot**: butuh akses GRUB dan root shell; tidak sesuai prinsip "semua lewat panel atau petunjuk".

## Konsekuensi

- NAS harus disimpan di tempat yang aman; konsol fisik memberi akses admin.
- Perubahan konfigurasi dari panel tepat saat reset jaringan bisa hilang (keduanya menulis `/etc/sapo/sapo.yaml`); kecil kemungkinannya karena panel tidak bisa dibuka saat reset dibutuhkan.
- Reset jaringan menghapus pengaturan jaringan lain di `/etc/network/interfaces` (bonding, VLAN, IP statis); cadangannya tetap ada. Bila nanti Sapo mengatur jaringan dari panel, reset ini ikut mengembalikan pengaturan itu.
