# ADR 0018: Notifikasi email

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

Backup terjadwal (ADR 0017), kesehatan pool, SMART, dan ruang pool hanya terlihat bila admin membuka panel. NAS rumahan dan kantor kecil jarang dibuka; masalah baru diketahui saat data sudah hilang. Admin butuh kabar lewat email, diatur sepenuhnya dari panel.

## Keputusan

- **sapo-api yang mengirim**, bukan `sapod`: mengirim email tidak mengubah sistem dan tidak butuh root (ADR 0002). Kondisi dibaca lewat operasi baca `sapod` yang sudah ada: `storage.pools` (kesehatan, kapasitas), `storage.disks` + `storage.smart` (disk yang tidur tidak dibangunkan), `backups.list`.
- **Pengaturan** di `<data_dir>/notify.json` (0600, milik akun `sapo`), lewat wizard `wizards/notify-setup.yaml` dan bagian Notifikasi di Control Panel. Semua perubahan butuh password admin dan tercatat di audit (tanpa password SMTP).
- **SMTP** (`internal/mail`, pustaka standar Go):
  - `starttls` wajib berhasil; server tanpa STARTTLS ditolak, tidak turun ke teks biasa.
  - `tls`: TLS langsung (biasanya port 465).
  - `none`: hanya untuk relay di jaringan lokal, dan login tidak boleh dipakai tanpa enkripsi.
  - Sertifikat server diperiksa dengan CA sistem.
  - Login dengan AUTH PLAIN atau LOGIN.
- **Password SMTP** tidak pernah dikirim kembali ke panel (`has_password` saja). Kosong saat menyimpan = tetap memakai yang tersimpan, **hanya** bila server, port, mode keamanan, dan username tidak berubah, sehingga password tersimpan tidak bisa diarahkan ke server lain. Password disimpan sebagai teks di file 0600 karena harus bisa dipakai untuk login; tidak ada kunci terpisah yang membuat enkripsi bermakna di mesin yang sama.
- **Email aman dari sisipan header**: alamat hanya alamat polos (`net/mail`, tanpa nama, koma, atau baris baru), subjek dikodekan RFC 2047 dan baris baru ditolak, isi UTF-8 quoted-printable, `Auto-Submitted: auto-generated`.
- **Kejadian** (`pool_health`, `pool_full`, `disk_smart`, `backup_failed`):
  - Diperiksa setiap `api.notify.check_minutes`, SMART setiap `smart_minutes`, dan segera setelah pengaturan diubah.
  - Satu email berisi semua masalah baru; masalah yang sama tidak dikirim ulang.
  - Email "pulih" dikirim saat masalah hilang.
  - Kejadian hanya dianggap pulih bila sumbernya berhasil dibaca: sapod yang tidak menjawab atau belum mengenal operasinya bukan "pulih", dan masalah disk hanya pulih bila SMART disk itu terbaca lulus (disk yang tidur, tidak terbaca, atau hilang tetap bermasalah).
  - Pool penuh baru pulih 2 poin di bawah ambang (histeresis).
  - Email yang sudah diterima server dianggap terkirim walau QUIT gagal; bila catatan notifikasi tidak bisa disimpan, email ditunda (tidak dikirim berulang).
  - Email yang gagal dikirim dicoba lagi pada pemeriksaan berikutnya, dan galatnya terlihat di panel.
  - Ambang pool penuh dan jenis kejadian bisa diatur admin.
- Nama host di email dibaca dari `sapod` (sapo-api berjalan dengan `ProtectHostname`).
- **Teks email** di `api.notify.messages` (`config/sapo.yaml`) per bahasa, dalam bahasa setup awal.

## Akibat

- Pembaruan sistem belum dikabarkan lewat email: memeriksanya butuh `apt-get update` berkala, menyusul bersama jadwal pemeriksaan pembaruan.
- Satu saluran (email). Saluran lain (Telegram, webhook) bisa ditambahkan di notifier yang sama.
- Uji asap ISO menjalankan server SMTP kecil di host: email uji, pool hampir penuh (ambang diturunkan), dan email pulih harus diterima; login tanpa enkripsi ditolak.
