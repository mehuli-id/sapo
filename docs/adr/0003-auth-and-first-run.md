# ADR 0003: Autentikasi, setup awal, dan akses jaringan

- **Status:** Diterima
- **Tanggal:** 2026-10-23

## Konteks

Panel tidak boleh terbuka ke jaringan sebelum ada login. Pengguna juga ingin semua setup dilakukan lewat web dengan wizard, tanpa terminal, termasuk membuat akun pertama.

## Keputusan

- **Setup awal lewat web.** Selama belum ada administrator, panel menampilkan wizard setup (`wizards/setup.yaml`). Wizard hanya bisa diselesaikan sekali; setelah itu endpoint setup menolak dengan `409`.
- **Password** di-hash dengan Argon2id (parameter di `auth.argon2`, `sapo.yaml`). Hash menyimpan parameternya sendiri, jadi parameter bisa dinaikkan tanpa memutus akun lama.
- **2FA (TOTP, RFC 6238) wajib untuk administrator** (`auth.require_totp_for_admin`). Kode yang sudah dipakai tidak bisa dipakai ulang.
- **Sesi** disimpan di memori sebagai hash SHA-256 dari token acak 256 bit; cookie `HttpOnly`, `SameSite=Strict`, `Secure` bila HTTPS. Batas idle dan batas maksimal diatur di konfigurasi.
- **CSRF:** setiap permintaan yang mengubah data wajib membawa header `X-Sapo-Request: 1`; bila ada header `Origin`, host-nya harus sama dengan host panel.
- **Batas percobaan login** tiga lapis, semua angka di konfigurasi:
  - per jaringan klien (IPv4 per alamat, IPv6 per /64), dipesan atomik sebelum password diperiksa;
  - per akun, hanya untuk akun yang ada, dan tidak bisa terhapus oleh banjir entri lain;
  - kode 2FA salah per akun (`auth.totp_max_failures` per `auth.totp_window_minutes`).
  Password yang benar tidak dihitung sebagai percobaan gagal, sehingga salah ketik kode 2FA tidak mengunci admin. Saat akun terkunci, hanya permintaan berisi password **dan** kode yang diterima, dan semua kegagalan dijawab sama (tidak ada petunjuk apakah password benar).
- **Hash Argon2 dibatasi** jumlah yang berjalan bersamaan (`auth.max_concurrent_hashes`) agar login massal tidak menghabiskan RAM.
- **Header `Host` divalidasi** (alamat IP, `localhost`, nama host mesin, `<nama host>.local`, dan `api.hostnames`) untuk mencegah DNS rebinding, yaitu situs lain yang mengarahkan domainnya ke IP NAS lalu memakai browser korban.
- **Akses jaringan:** permintaan hanya diterima dari `api.allowed_networks` (default: localhost dan LAN privat), dikurangi `api.denied_networks` (default: jaringan Docker bawaan). Akses dari internet tidak dilakukan dengan memperluas daftar ini, tetapi lewat fitur "Akses dari internet" (VPN/tunnel) di fase berikutnya.
- **Log audit** (`<data_dir>/audit.log`, 0600) mencatat setup, login berhasil/gagal, dan logout; dirotasi saat mencapai `api.audit_max_mb`.
- **Penyimpanan user sementara** berupa satu file JSON (0600) dengan penulisan atomik. Antarmukanya sempit agar bisa diganti SQLite tanpa mengubah pemanggil.

## Konsekuensi dan batasan yang diketahui

- HTTPS wajib di jaringan lokal sejak ADR 0004; sebelumnya password dikirim tanpa enkripsi di LAN.
- Selama setup belum selesai, siapa pun di LAN bisa membuka wizard setup. Ini sama dengan perilaku NAS komersial; jendela waktunya sempit (hanya sampai admin pertama dibuat).
- Sesi hilang saat `sapo-api` di-restart; pengguna cukup login ulang.
- Rentang Tailscale/CGNAT (100.64.0.0/10) tidak dibuka secara default; integrasi Tailscale nanti menambahkannya.
- Daftar jaringan bukan pengganti login: port forward atau hairpin NAT di router bisa membuat klien internet terlihat seperti alamat LAN.
- Token setup sekali pakai (ditampilkan di layar NAS) dipertimbangkan sebagai lapisan tambahan untuk setup awal, tetapi belum dipakai karena bertentangan dengan prinsip "semua lewat web"; validasi Host sudah menutup serangan DNS rebinding.
- Akun yang sedang terkunci bisa dibedakan dari nama yang tidak ada (jawaban "masukkan kode"). Ini dianggap risiko kecil dibanding membuat entri limiter untuk setiap nama acak.
- **Pemulihan akses 2FA** (sejak 2026-10-24):
  - **Kode cadangan.** Setup membuat `auth.recovery_code_count` kode cadangan sekali pakai (bawaan 10), dengan format `XXXXX-XXXXX` (Crockford base32, 50 bit). Kode ditampilkan sekali di langkah wizard yang wajib dikonfirmasi, dan hanya hash SHA-256-nya yang disimpan.
  - **Login dengan kode cadangan.** Kode dimasukkan di kolom kode 2FA dan dihitung di penghitung kode 2FA salah yang sama. Pemakaiannya dicatat di log audit beserta sisa kode.
  - **Aplikasi Keamanan Akun.** Admin bisa membuat kode baru (semua kode lama tidak berlaku) dan memindahkan 2FA ke aplikasi autentikator baru. Rahasia lama tetap berlaku sampai kode dari aplikasi baru dikonfirmasi. Memindahkan 2FA tidak mengganti kode cadangan, dan panel menyatakan hal itu.
  - **Wajib password lagi** untuk kedua tindakan itu. Password salah dihitung di batas tersendiri (`reauth:<user>`), bukan batas login, supaya orang lain yang mengunci login akun tidak bisa menghalangi pemilik memutar 2FA saat insiden.
  - **Sesi lain berakhir.** Setelah salah satu tindakan itu berhasil, semua sesi lain milik user diakhiri. Sesi yang sedang dipakai tetap berlaku.
  - **Peringatan.** Panel memberi peringatan bila sisa kode `<= auth.recovery_codes_low`.
  - **Belum ada:** pemulihan bila password **dan** kode cadangan sama-sama hilang. Rencananya lewat menu darurat di konsol NAS (dokumen rancangan).
- **Pengguna dan grup** (sejak 2026-10-24, aplikasi Pengguna & Grup):
  - **Dua peran.** `admin` mengelola NAS. `user` hanya memakai file dan aplikasi yang diizinkan; storage, paket, serta pengguna dan grup hanya untuk admin, dan hak itu diperiksa server (`requireAdmin`), bukan hanya disembunyikan di panel.
  - **Grup** dipakai untuk izin folder dan aplikasi. Nama grup mengikuti `auth.group_name_pattern` dan tidak boleh memakai `auth.reserved_group_names`; nama pengguna tidak boleh memakai `auth.reserved_usernames`. Keduanya nantinya juga menjadi akun dan grup sistem untuk berbagi file, jadi nama sistem Debian dicadangkan sejak awal.
  - **Password admin diminta lagi** untuk tindakan yang memberi hak atau mencabut akses: membuat user, mengubah peran, me-reset password, dan menghapus user. Menghapus juga butuh nama diketik ulang.
  - **Sesi berakhir** saat peran user berubah, akunnya dinonaktifkan, password-nya di-reset admin, atau akunnya dihapus. User yang dinonaktifkan tidak bisa login; pesannya baru diberikan setelah password terbukti benar.
  - **Selalu ada administrator aktif.** Penyimpanan menolak perubahan yang membuat NAS tanpa admin aktif, dan admin tidak bisa menonaktifkan, menurunkan, atau menghapus akunnya sendiri.
  - **Langkah wajib akun.** Password yang dibuat atau di-reset admin wajib diganti saat masuk (`must_change_password`), dan administrator tanpa 2FA wajib memasangnya. Selama langkah ini belum selesai, server menolak semua API selain status, logout, ganti password, dan pendaftaran 2FA (`account_setup_required`).
  - **Ganti password sendiri** di Keamanan Akun, dengan password lama; sesi lain berakhir.
  - Semua perubahan pengguna dan grup dicatat di log audit, tanpa password.
