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
  - `ProtectSystem=strict`, `NoNewPrivileges`, `PrivateTmp`/`PrivateDevices`/`PrivateIPC`, `Protect*`, `RestrictNamespaces`, `MemoryDenyWriteExecute`, dan `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`;
  - backup: `CapabilityBoundingSet=CAP_DAC_READ_SEARCH` (membaca semua file, tidak bisa menulis selain cache);
  - perintah baca: tanpa kapabilitas;
  - pemulihan: kapabilitas pemilik file, dan hanya boleh menulis ke folder baru tujuan pemulihan (`ReadWritePaths`).
  - Unit dihentikan (`systemctl stop`) bila operasi dibatalkan.
- **Rahasia.**
  - Password repo dan kunci S3 hanya disimpan sapod di `agent.cloud.secrets_dir` (0600, root).
  - Keduanya masuk ke unit lewat `LoadCredential` (`--password-file` dan `AWS_SHARED_CREDENTIALS_FILE` menunjuk direktori kredensial unit), tidak pernah lewat argumen atau variabel lingkungan, yang bisa dibaca pengguna mana pun lewat `systemctl show`.
  - `cloud.list` dan sapo-api tidak pernah mengembalikan rahasia; log audit mencatat server dan bucket saja.
- **Password enkripsi.** Repo baru diberi password acak 160 bit (base32 berkelompok), ditampilkan **sekali** di panel. Admin wajib menyatakan sudah mencatatnya di luar NAS sebelum kotak itu ditutup. Tanpa password itu, backup tidak bisa dibuka siapa pun. "Sambungkan backup cloud yang ada" memeriksa password (`restic cat config`) dan dipakai di NAS pengganti setelah NAS lama rusak. Repo kedua di tempat yang sama ditolak (`cloud_repo_exists`).
- **Backup konsisten.**
  - Snapshot ZFS sementara bernama tetap (`@sapo-cloud-<tugas>`) dibuat, dibaca restic, lalu dibuang. Path yang sama setiap kali membuat restic hanya membaca file yang berubah. Snapshot dipasang (automount) di namespace sapod sebelum unit dimulai.
  - Tag `sapo`, `job:<id>`, `share:<nama>`. Retensi `forget --keep-within <N>d --keep-last 1 --prune` per tugas (yang terbaru selalu tetap).
  - Satu restic pada satu waktu. Jadwal harian (waktu NAS), percobaan gagal diulang paling cepat sejam kemudian.
- **Data dulu.**
  - Backup tidak mengubah folder asal.
  - Menghapus tugas atau "melupakan" repo tidak menghapus data di cloud.
  - Shared folder dan pool yang dipakai tugas tidak bisa dihapus.
  - Pemulihan selalu ke shared folder **baru**: dataset dibuat sapod, dan bila pemulihan gagal, dataset baru itu dibuang lagi. Pendaftaran folder lewat mekanisme klaim yang sama dengan ADR 0017.
  - Mengubah atau menghapus tugas, menambah atau melupakan repo, dan memulihkan butuh password admin dan tercatat di audit.
  - Backup cloud yang gagal memicu email `backup_failed` (ADR 0018). Masalah cloud hanya dianggap pulih bila daftar cloud terbaca.
- `insecure_http` hanya untuk server S3 di jaringan lokal (misalnya MinIO). Data tetap terenkripsi restic, tetapi permintaan S3 tidak terlindungi TLS.

## Akibat

- Riwayat snapshot ZFS dan properti dataset tidak ikut ke cloud; pemulihan menghasilkan folder dengan isi file pada saat backup.
- Unggahan pertama bisa lama bergantung kecepatan internet; batas `agent.cloud.max_run_hours`.
- Uji asap ISO memakai MinIO di host (Docker): repo baru, repo ganda dan password salah ditolak, backup penuh dan inkremental, isi file tidak terbaca di server S3, pemulihan bencana (repo dilupakan lalu disambung lagi hanya dengan password) ke folder baru, dan pembacaan lewat SMB.
