# ADR 0013: Sapo Drive (tahap 1: folder pribadi)

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
- Tahap 2: WebDAV di port HTTPS panel untuk folder pribadi dan shared folder sesuai izin, beserta petunjuk menghubungkan Windows, macOS, iOS, Android, dan rclone.
