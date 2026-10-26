# Architecture Decision Records

Setiap keputusan arsitektur penting dicatat di sini sebagai file bernomor (`NNNN-judul.md`) dengan bagian: Status, Konteks, Keputusan, Alasan, Alternatif, Konsekuensi.

| No. | Keputusan | Status |
|---|---|---|
| [0001](0001-base-os-debian.md) | Debian stable minimal sebagai base OS | Diterima |
| [0002](0002-agent-api-separation.md) | Pemisahan `sapod` (root) dan `sapo-api` lewat operasi bernama | Diterima |
| [0003](0003-auth-and-first-run.md) | Autentikasi, setup awal lewat web, dan akses jaringan | Diterima |
| [0004](0004-local-https.md) | HTTPS wajib di jaringan lokal dengan sertifikat self-signed | Diterima |
| [0005](0005-zfs-pools.md) | Pool penyimpanan ZFS dari panel | Diterima |
| [0006](0006-shared-folders-smb.md) | Shared folder, akun sistem, dan SMB | Diterima |
| [0007](0007-nfs.md) | Shared folder lewat NFS (v4, semua akses sebagai satu pengguna) | Diterima |
| [0008](0008-console-recovery.md) | Menu darurat di konsol NAS (reset password admin, jaringan, restart) | Diterima |
| [0009](0009-network-settings.md) | Pengaturan jaringan dari panel dengan konfirmasi dan pengembalian otomatis | Diterima |
| [0010](0010-file-station.md) | File Station: operasi file sebagai akun pengguna, openat2, file descriptor | Diterima |
| [0011](0011-system-updates.md) | Pembaruan sistem dari panel di unit systemd tersendiri | Diterima |
| [0012](0012-app-center.md) | App Center: Podman, katalog kurasi, akun per aplikasi, Quadlet | Diterima |
| [0013](0013-sapo-drive.md) | Sapo Drive: folder pribadi per pengguna (dataset ZFS, SMB [homes], File Station) dan WebDAV dengan password aplikasi | Diterima |
| [0014](0014-snapshots.md) | Snapshot ZFS terjadwal per folder (jadwal di properti dataset, Previous Versions lewat SMB) | Diterima |
| [0015](0015-external-storage.md) | Penyimpanan eksternal USB (exFAT/FAT/NTFS, izin per grup saat dipasang, disk USB tidak untuk pool kecuali diizinkan) | Diterima |
| [0016](0016-installer.md) | Installer ke disk dari panel (salin squashfs ISO, root ext4, GRUB BIOS+UEFI, pengaturan sesi ikut) | Diterima |
| [0017](0017-backup.md) | Backup shared folder ke pool lain (zfs send/receive harian, salinan tidak dibagikan, pemulihan ke folder baru) | Diterima |
| [0018](0018-email-notifications.md) | Notifikasi email dari sapo-api (SMTP STARTTLS/TLS, satu email per masalah baru, email pulih, password SMTP tidak pernah dikembalikan) | Diterima |
| [0019](0019-remote-backup.md) | Backup ke NAS Sapo lain lewat HTTPS sapo-api (tanpa SSH), kode pemasangan dengan sidik jari, partner tidak bisa menghapus salinan | Diterima |
| [0020](0020-cloud-backup.md) | Backup ke S3/cloud dengan restic (enkripsi di NAS, unit systemd terkunci, password tampil sekali, pemulihan bencana dengan password) | Diterima |
| [0021](0021-recycle-bin.md) | Tempat sampah per shared folder (`#recycle`: SMB vfs_recycle, File Station/WebDAV memindah, dibersihkan sapod lewat fd tanpa symlink) | Diterima |
