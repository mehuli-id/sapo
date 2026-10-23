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
- **Batas percobaan login** per IP dan per nama pengguna, dipesan secara atomik sebelum password diperiksa (permintaan paralel tidak bisa melewati batas). Admin dengan 2FA tetap bisa masuk dengan password + kode yang benar walau akunnya sedang "dikunci" oleh percobaan orang lain; kunci per IP tetap berlaku. Semua angka di konfigurasi.
- **Hash Argon2 dibatasi** jumlah yang berjalan bersamaan (`auth.max_concurrent_hashes`) agar login massal tidak menghabiskan RAM.
- **Header `Host` divalidasi** (alamat IP, `localhost`, nama host mesin, `<nama host>.local`, dan `api.hostnames`) untuk mencegah DNS rebinding, yaitu situs lain yang mengarahkan domainnya ke IP NAS lalu memakai browser korban.
- **Akses jaringan:** permintaan hanya diterima dari `api.allowed_networks` (default: localhost dan LAN privat), dikurangi `api.denied_networks` (default: jaringan Docker bawaan). Akses dari internet tidak dilakukan dengan memperluas daftar ini, tetapi lewat fitur "Akses dari internet" (VPN/tunnel) di fase berikutnya.
- **Log audit** (`<data_dir>/audit.log`, 0600) mencatat setup, login berhasil/gagal, dan logout.
- **Penyimpanan user sementara** berupa satu file JSON (0600) dengan penulisan atomik. Antarmukanya sempit agar bisa diganti SQLite tanpa mengubah pemanggil.

## Konsekuensi dan batasan yang diketahui

- Sampai HTTPS bawaan tersedia, password dikirim tanpa enkripsi di LAN (sama seperti NAS komersial pada setelan awal). HTTPS lokal dengan sertifikat yang dibuat otomatis dijadwalkan berikutnya.
- Selama setup belum selesai, siapa pun di LAN bisa membuka wizard setup. Ini sama dengan perilaku NAS komersial; jendela waktunya sempit (hanya sampai admin pertama dibuat).
- Sesi hilang saat `sapo-api` di-restart; pengguna cukup login ulang.
- Rentang Tailscale/CGNAT (100.64.0.0/10) tidak dibuka secara default; integrasi Tailscale nanti menambahkannya.
- Daftar jaringan bukan pengganti login: port forward atau hairpin NAT di router bisa membuat klien internet terlihat seperti alamat LAN.
- Token setup sekali pakai (ditampilkan di layar NAS) dipertimbangkan sebagai lapisan tambahan untuk setup awal, tetapi belum dipakai karena bertentangan dengan prinsip "semua lewat web"; validasi Host sudah menutup serangan DNS rebinding.
- Pemulihan akses bila HP 2FA hilang belum ada; untuk sementara kunci rahasia ditampilkan saat setup agar bisa disimpan sebagai cadangan.
