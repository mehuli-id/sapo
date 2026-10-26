# ADR 0020: Backup ke S3/cloud dengan restic

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

Backup ke pool lain (ADR 0017) dan ke NAS lain (ADR 0019) butuh disk atau NAS kedua. Kebanyakan pengguna Sapo di rumah dan kantor kecil hanya punya satu NAS, padahal salinan di luar lokasi melindungi dari kebakaran, pencurian, dan banjir. Penyimpanan S3 (Backblaze B2, Wasabi, Cloudflare R2, AWS, MinIO) murah dan tersedia di mana-mana, tetapi data tidak boleh terbaca oleh penyedia.

## Keputusan

- **restic** (paket Debian, fitur opsional `cloud-backup`), bukan kode buatan sendiri dan bukan `zfs send` ke objek.
  - restic mengenkripsi di NAS (AES-256 + Poly1305), menyimpan bagian yang sama sekali saja (deduplikasi), dan bisa memangkas salinan lama.
  - Rantai `zfs send` inkremental di objek tidak bisa dipangkas tanpa mengunggah ulang salinan penuh.
  - Yang dicadangkan adalah isi file: ACL POSIX dan atribut ikut sebagai xattr. Riwayat snapshot ZFS tidak ikut.
- **sapod tidak ke jaringan.** sapod menjalankan restic lewat `systemd-run --wait --pipe` di unit sementara yang dikunci:
  - restic **bukan root**: `DynamicUser=yes`, kapabilitas hanya lewat `AmbientCapabilities` sesuai mode;
  - `ProtectSystem=strict`, `NoNewPrivileges`, `PrivateTmp`/`PrivateDevices`/`PrivateIPC`, `Protect*`, `RestrictNamespaces`, `MemoryDenyWriteExecute`;
  - `RestrictAddressFamilies=AF_INET AF_INET6`, **tanpa socket unix**: pengguna dengan `CAP_DAC_OVERRIDE` bisa menyambung ke socket mana pun, termasuk D-Bus/systemd/sapod (jalur ke root). Resolver Go murni (`GODEBUG=netdns=go`);
  - `InaccessiblePaths` untuk rahasia dan state sapod, data sapo-api (`/var/lib/sapo`, `/etc/sapo`), `/etc/shadow`, `/run/sapo`, `/run/dbus`, `/var/lib/samba`. `/run/systemd` tidak bisa disembunyikan (systemd memakai `/run/systemd/inaccessible` untuk `InaccessiblePaths`); socketnya tetap tak terjangkau karena unit tanpa AF_UNIX;
  - cache restic di `CacheDirectory` milik pengguna dinamis;
  - backup: `CAP_DAC_READ_SEARCH` saja (membaca semua file, tidak bisa menulis apa pun selain cache);
  - perintah baca (`init`, `snapshots`, `ls`, `forget`): tanpa kapabilitas;
  - pemulihan: `CAP_CHOWN CAP_FOWNER CAP_DAC_OVERRIDE`, dan hanya boleh menulis ke folder baru tujuan pemulihan (`ReadWritePaths`).
  - Unit dihentikan (`systemctl stop`) bila operasi dibatalkan.
  - Risiko sisa: dataset yang dipasang di host *selama* pemulihan berjalan mungkin ikut terlihat bisa ditulis di namespace unit (propagasi mount); pemulihan jarang dan dimulai admin.
- **Rahasia.**
  - Password repo dan kunci S3 hanya disimpan sapod di `agent.cloud.secrets_dir` (0600, root).
  - Keduanya masuk ke unit lewat `LoadCredential` (`--password-file` dan `AWS_SHARED_CREDENTIALS_FILE` menunjuk direktori kredensial unit), tidak pernah lewat argumen atau variabel lingkungan, yang bisa dibaca pengguna mana pun lewat `systemctl show`.
  - `cloud.list` dan sapo-api tidak pernah mengembalikan rahasia; log audit mencatat server dan bucket saja.
- **Password enkripsi.** Repo baru diberi password acak 160 bit (base32 berkelompok), ditampilkan **sekali** di panel. Admin wajib menyatakan sudah mencatatnya di luar NAS sebelum kotak itu ditutup. Tanpa password itu, backup tidak bisa dibuka siapa pun. "Sambungkan backup cloud yang ada" memeriksa password (`restic cat config`) dan dipakai di NAS pengganti setelah NAS lama rusak. Repo kedua di tempat yang sama ditolak (`cloud_repo_exists`).
- **Backup konsisten.**
  - Snapshot ZFS sementara bernama tetap (`@sapo-cloud-<tugas>`) dibuat, dibaca restic, lalu dibuang. Path yang sama setiap kali membuat restic hanya membaca file yang berubah. Snapshot dipasang (automount) di namespace sapod sebelum unit dimulai.
  - Tag `sapo`, `job:<id>`, `share:<nama>`, host = nama host NAS.
  - Retensi `forget --host <nama host> --group-by host,tags --keep-within <N>d --keep-daily 7 --keep-last 1 --prune`: per NAS dan tugas, paling sedikit 7 salinan harian. Pemangkasan **ditunda** bila jam NAS belum tersinkron NTP (kecuali NTP sengaja dimatikan), karena `--keep-within` dihitung dari salinan terbaru dan salinan bertanggal masa depan bisa membuat salinan asli terbuang.
  - Satu restic pada satu waktu. Jadwal harian (waktu NAS), percobaan gagal diulang paling cepat sejam kemudian.
- **Data dulu.**
  - Backup tidak mengubah folder asal.
  - Menghapus tugas atau "melupakan" repo tidak menghapus data di cloud.
  - Shared folder dan pool yang dipakai tugas tidak bisa dihapus.
  - Pemulihan selalu ke shared folder **baru**: dataset dibuat sapod, dan bila pemulihan gagal, dataset baru itu dibuang lagi.
  - **Pemilik dan ACL dari salinan dibuang** setelah pemulihan (`root:root`, tanpa ACL POSIX, tanpa setuid/setgid, tanpa mengikuti symlink, tanpa keluar dari dataset). UID di NAS lama bisa milik orang lain di NAS ini; izin lalu ditetapkan ulang dari izin shared folder. Ini juga berlaku untuk pemulihan salinan dari NAS lain (ADR 0019).
  - ID salinan harus unik (awalan yang cocok dengan beberapa salinan ditolak). Pendaftaran folder lewat mekanisme klaim yang sama dengan ADR 0017.
  - Mengubah atau menghapus tugas, menambah atau melupakan repo, dan memulihkan butuh password admin dan tercatat di audit.
  - Backup cloud yang gagal memicu email `backup_failed` (ADR 0018). Masalah cloud hanya dianggap pulih bila daftar cloud terbaca.
- **Pemulihan satu file atau folder** (tambahan 2026-10-26).
  - `cloud.ls` menampilkan isi satu folder di salinan (nama, jenis, ukuran, waktu; isi file tidak dibaca), paling banyak `agent.cloud.list_max_entries` isi. Hanya admin.
  - Path dari panel harus relatif, tanpa `.`/`..` atau komponen kosong. Akar salinan selalu diambil dari daftar salinan repo (disimpan 2 menit agar setiap klik folder tidak meminta daftar lagi), bukan dari panel.
  - Pemulihan tetap ke shared folder **baru**: `restic restore <id>:<akar> --include /<path>`, dengan karakter pola (`\ * ? [ ]`) di-escape sehingga hanya path itu yang cocok. Folder induknya ikut dibuat, jadi letak file sama seperti di folder asal. Bila path tidak ada di salinan, folder baru dibuang dan pemulihan dicatat gagal.
  - Pemulihan langsung ke folder yang sudah ada tidak disediakan: restic akan menulis sebagai root ke folder yang dipakai pengguna, dan pemilik serta ACL di sana tidak bisa di-reset tanpa menyentuh file lain. Admin memindahkan file dari folder baru lewat File Station.
- `insecure_http` hanya untuk server S3 di jaringan lokal (misalnya MinIO). Data tetap terenkripsi restic, tetapi permintaan S3 tidak terlindungi TLS.

## Akibat

- Riwayat snapshot ZFS dan properti dataset tidak ikut ke cloud; pemulihan menghasilkan folder dengan isi file pada saat backup.
- Unggahan pertama bisa lama bergantung kecepatan internet; batas `agent.cloud.max_run_hours`.
- Uji asap ISO memakai server S3 SeaweedFS di host (Docker): repo baru, repo ganda dan password salah ditolak, backup penuh dan inkremental, isi file tidak terbaca di server S3, pemulihan bencana (repo dilupakan lalu disambung lagi hanya dengan password) ke folder baru, dan pembacaan lewat SMB; penelusuran isi salinan (subfolder dengan `[ ]` di nama, path `..` ditolak) dan pemulihan satu file ke folder baru tanpa file lain.
