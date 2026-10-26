# ADR 0021: Tempat sampah shared folder

- **Status:** Diterima
- **Tanggal:** 2026-10-26

## Konteks

Salah hapus adalah penyebab kehilangan data paling sering di rumah dan kantor kecil. Snapshot (ADR 0014) bisa mengembalikan file, tetapi hanya bila snapshot sudah diambil sebelum file dihapus. Orang awam juga lebih mudah memahami "tempat sampah" daripada "versi sebelumnya". Synology dan QNAP punya tempat sampah per shared folder (`#recycle`).

## Keputusan

- **Per shared folder**, lama simpan dalam hari (1–365, `0` = mati), disimpan bersama folder di `shares.json` dan diteruskan ke `sapod` lewat `shares.apply`.
  - `sapod` menyimpannya di properti ZFS `sapo:recycle`, seperti jadwal snapshot.
  - Folder baru langsung punya tempat sampah: `api.recycle_default_days`, bawaan 30. Folder lama tidak berubah sampai admin menyalakannya.
  - Mengubah lama simpan butuh password admin dan tercatat di audit, karena lama simpan yang lebih pendek membuang isi lebih cepat.
- **Folder `#recycle` di akar folder**, dengan letak yang sama seperti di folder asal.
  - Dibuat `sapod` lewat fd akar folder (`mkdirat`, tanpa mengikuti symlink), sehingga izinnya mewarisi ACL bawaan folder dan tidak dimiliki pengguna yang pertama menghapus.
  - Bila `#recycle` tidak bisa dibuat (misalnya nama itu dipakai file), tempat sampah tidak dinyalakan di Samba dan panel menampilkan peringatan. Tanpa folder itu, `vfs_recycle` akan menghapus langsung.
  - Isi tempat sampah tetap memakai ACL folder: pengguna yang bisa membaca folder bisa melihat dan mengambil kembali isinya. Memulihkan = memindahkan file kembali (File Station atau SMB). Tidak ada pemulihan khusus di tahap ini.
- **SMB**: Samba `vfs_recycle` per share (`recycle:repository = #recycle`, `keeptree`, `versions`, mode folder `0770` agar ACL bawaan berlaku).
  - Pola file sementara aplikasi (`agent.recycle.exclude`, misalnya `~$*`, `*.tmp`) dihapus langsung. Pola diperiksa sebelum ditulis ke `smb.conf`.
- **File Station dan WebDAV**: `files.delete` di proses pembantu (sebagai akun pengguna, ADR 0010) memindah ke `#recycle/<folder induk>/<nama>` dengan `renameat2(RENAME_NOREPLACE)`.
  - Folder perantara dibuat dengan `openat2` tanpa symlink dan tanpa pindah sistem berkas.
  - Nama yang sudah ada di tempat sampah tidak ditimpa: diberi akhiran `agent.files.deleted_label` (`dihapus {time}`).
  - Bila tempat sampah tidak bisa dipakai, penghapusan **gagal**; file tidak pernah dihapus langsung diam-diam.
- **Hapus di dalam `#recycle` = permanen** (SMB dan File Station). Tempat sampah tidak pernah masuk ke dirinya sendiri.
- **Pembersihan**: `sapod` memeriksa semua tempat sampah setiap `agent.recycle.check_minutes`.
  - Membuang file, symlink, dan folder kosong yang **ctime**-nya lebih lama dari lama simpan. ctime berubah saat dipindah ke tempat sampah, jadi itu waktu dihapus. Penerapan ulang izin folder (ACL) juga mengubah ctime, sehingga isi tempat sampah bisa tersimpan lebih lama dari lama simpan, tidak pernah lebih pendek.
  - `sapod` berjalan sebagai root di pohon yang bisa diubah pengguna. Karena itu setiap nama dibuka relatif ke fd folder induknya (`openat2` dengan `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_XDEV`) dan dihapus dengan `unlinkat`. Tidak ada path yang diikuti, tidak ada symlink yang diikuti, dan sistem berkas lain tidak dimasuki.
  - Tempat sampah yang dimatikan tidak dibuang isinya, hanya dihitung.
  - Isi (byte, jumlah file) dicatat untuk panel (`recycle.status`).
- **Kosongkan sekarang** (`recycle.empty`): nama folder diketik ulang, password admin, log audit. Dibatasi `agent.recycle.empty_timeout_seconds`; bila waktu habis, sebagian sudah terhapus dan admin bisa mengulang.

## Akibat

- Folder pribadi (Sapo Drive) dan penyimpanan eksternal belum punya tempat sampah. Penimpaan file (unggah dengan nama sama, WebDAV `MOVE`/`PUT` yang menimpa) tidak masuk tempat sampah, sama dengan perilaku `vfs_recycle`.
- Isi tempat sampah ikut snapshot, backup, dan kuota folder.
- Uji asap ISO: hapus lewat File Station dan SMB pindah ke `#recycle` dengan letak yang sama dan bisa diambil lagi lewat SMB, hapus di dalamnya permanen, kosongkan dengan password, dan dimatikan berarti hapus langsung.
