# ADR 0013: Sapo Drive

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

Pengguna NAS ingin tempat file pribadi yang hanya bisa dibuka dirinya sendiri, seperti "home" di Synology. Pilihan yang disepakati untuk sinkronisasi adalah **WebDAV** (didukung bawaan Windows, macOS, iOS, Android, dan klien seperti rclone), bukan klien sinkron buatan sendiri atau Nextcloud. Tahap 1 menyiapkan folder pribadinya; WebDAV menyusul di tahap 2.

## Keputusan

- **Admin menyalakan Sapo Drive lewat wizard** (`wizards/drive-enable.yaml`): memilih satu pool dan batas ukuran bawaan per pengguna (GB, kosong = tanpa batas). Pengaturan disimpan sapo-api (`settings.json`: `homes_pool`, `homes_quota_gb`, `home_quota_gb` per pengguna, -1 = tanpa batas). Password admin dan log audit (`drive.settings`, `drive.quota`).
- **Setiap pengguna Sapo mendapat dataset** `<pool>/sapo-home/<nama>` (penanda `sapo:home=<nama>`, `devices=off`, `setuid=off`) milik akun Linux-nya dengan mode 0700, terpasang di `<mount_base>/<pool>/sapo-home/<nama>`. Induknya `<pool>/sapo-home` (penanda `sapo:home=@induk`) milik root dengan mode 0711: pengguna bisa masuk ke foldernya sendiri tetapi tidak bisa melihat daftar folder lain. Administrator juga tidak bisa membuka folder pribadi orang lain. Kuota memakai properti ZFS `refquota`, agar snapshot (terjadwal, menyusul) tidak memakan kuota pengguna. Folder yang sudah ada hanya dipakai bila pemiliknya memang akun pengguna itu (UID tidak pernah dipakai ulang), jadi folder sisa pengguna lama bernama sama, misalnya di pool yang diimpor lagi, tidak pernah diserahkan ke akun baru.
- **Disiapkan sebagai bagian sinkronisasi** (`homes.apply` di `sapod`, keadaan lengkap, setelah `accounts.sync` dan sebelum `shares.apply`), hanya untuk akun yang tercatat dibuat `sapod`. Nama berawalan `sapo-` ditolak. Dataset bernama sama tanpa penanda tidak pernah dipakai.
- **Dibuka lewat**:
  - SMB `[homes]` (`\\nas\<nama>`): `path` ke folder pribadi, `valid users = %S`, tidak tampil di daftar share, mati bila SMB dimatikan di Control Panel, dan hanya ditulis bila folder induknya berhasil dipastikan milik Sapo Drive;
  - File Station sebagai "Drive saya" (`FilesTarget.Home`): `sapod` selalu memakai folder milik pengguna sesi itu sendiri, dan operasinya tetap berjalan sebagai akun pengguna (ADR 0010).
- **Data dulu**:
  - folder tidak pernah dihapus karena penggunanya dihapus atau Sapo Drive dimatikan; folder itu tampil sebagai "folder sisa";
  - penghapusan hanya untuk pengguna yang sudah tidak ada (panel dan `sapod` memeriksa), dengan nama diketik ulang, password admin, dan log audit (`drive.home_delete`);
  - pengguna baru tidak bisa memakai nama yang masih punya folder sisa, agar tidak mewarisi data orang lain;
  - pool tidak bisa diganti selama masih ada folder pribadi di pool lain;
  - pool yang dihapus mematikan Sapo Drive.
- Nama shared folder `sapo-home` dicadangkan.
- **Penanda dataset induk** (`sapo-home`, dan `sapo-apps` App Center) bernilai `@induk`, bukan `-`: properti pengguna ZFS yang tidak diset terbaca `-`, jadi nilai `-` akan menerima dataset buatan luar Sapo. `sapo-apps` dari App Center tahap 1 (penanda `-` yang diset langsung di dataset itu) dipindah otomatis ke `@induk`.

## Konsekuensi

- Folder pribadi tidak dipindah otomatis antar-pool; memindahkan data menunggu fitur salin/pindah antar-pool.
- Kuota yang diturunkan di bawah pemakaian tidak menghapus file; pengguna hanya tidak bisa menambah file.

## Tahap 2: WebDAV (2026-10-25)

- **`https://<nas>/dav/`** di port HTTPS panel (sapo-api, `golang.org/x/net/webdav`). Akarnya berisi `@home` (Drive saya) dan shared folder yang boleh dibuka pengguna, dengan izin yang sama dengan File Station. Akar `/dav/` dan akar setiap folder tidak bisa diubah.
- **Setiap operasi lewat `sapod fs-helper` sebagai akun pengguna** (ADR 0010): daftar isi, `files.stat` (baru, `O_PATH` tanpa mengikuti symlink), baca dan tulis lewat file descriptor. Symlink dan berkas khusus tidak tampil. Tulis selalu ke file sementara; namanya baru diberikan (menimpa file lama) bila isi permintaan terbaca utuh, jadi unggahan yang terputus tidak pernah merusak file lama. Pindah hanya di dalam satu folder.
- **Login: nama pengguna dan password aplikasi** (HTTP Basic, hanya lewat HTTPS; HTTP biasa ditolak). Password aplikasi dibuat pengguna sendiri di Keamanan Akun:
  - satu per perangkat, dibuat dengan password akun;
  - rahasia acak sekitar 75 bit, disimpan sebagai SHA-256, dan ditampilkan sekali saja;
  - paling banyak 20 per akun, dan bisa dicabut satu per satu;
  - semuanya dicabut bila password akun diganti atau di-reset;
  - setiap pembuatan dan pencabutan dicatat di log audit.

  Password akun tidak berlaku untuk WebDAV, dan password aplikasi tidak berlaku untuk panel, jadi 2FA panel tetap utuh. Akun nonaktif atau yang wajib mengganti password ditolak.
- **Pembatasan**:
  - jaringan yang diizinkan dan validasi Host sama dengan panel;
  - percobaan login gagal dibatasi limiter (kunci tersendiri per jaringan klien dan per akun) dan dicatat di log audit;
  - tanpa cookie, jadi tidak butuh header CSRF; permintaan dengan Origin lain ditolak;
  - menulis ke folder baca saja dijawab 403 sebelum diteruskan;
  - unduhan selalu `application/octet-stream`, lampiran, dan `CSP: sandbox`;
  - perubahan (PUT, DELETE, MKCOL, MOVE, COPY) dicatat di log audit;
  - admin bisa mematikan WebDAV di Control Panel.
- **Sertifikat**: sertifikat HTTPS panel (bukan rahasia) bisa diunduh dari Sapo Drive (`/api/v1/tls/certificate`) untuk dipercaya di perangkat lain. Petunjuk di Sapo Drive tidak pernah menyarankan mematikan pemeriksaan sertifikat.
- **Petunjuk di Sapo Drive** untuk Windows, macOS, iOS, Android, dan rclone (`rclone bisync` untuk sinkron dua arah).
- **Kunci WebDAV** (LOCK) di memori sapo-api, hilang saat sapo-api di-restart:
  - path `@home` dipisah per pengguna;
  - token acak dan hanya berlaku untuk pemiliknya (tidak bisa dipakai atau dicabut pengguna lain);
  - lama kunci paling lama 1 jam, paling banyak 200 kunci per pengguna;
  - LOCK pada akar `/dav/`, akar folder, dan folder baca saja ditolak.
- **Penimpaan aman**:
  - MOVE dan COPY yang menimpa file tidak menghapus tujuan dulu; tujuan ditimpa atomik oleh `sapod` (rename dengan `Overwrite`, atau commit file sementara), jadi bila operasinya gagal, file lama tetap ada;
  - file baru tanpa isi (misalnya LOCK pada nama yang gagal dibaca karena `sapod` sibuk) tidak pernah menggantikan file lama;
  - MOVE antar-folder dijawab 502 sebelum apa pun dihapus (klien menyalin lalu menghapus);
  - COPY dan MOVE ke dalam dirinya sendiri ditolak.
- **Daftar isi yang terpotong** (lebih dari `agent.files.max_entries`) tidak pernah dikirim sebagai daftar lengkap: PROPFIND dijawab 507, agar klien sinkron tidak menganggap file sisanya terhapus. Folder sebesar itu belum bisa dibuka lewat WebDAV; paging menyusul.
- **Batas lain**:
  - PROPFIND hanya Depth 0 dan 1 (RFC 4918 §9.1);
  - badan permintaan selain PUT paling besar 1 MiB;
  - limiter login WebDAV hanya menghitung kegagalan, dengan kunci per jaringan klien dan per (jaringan, akun), jadi tebakan dari jaringan lain tidak mengunci pengguna yang sah;
  - waktu pakai password aplikasi dicatat di latar belakang.
