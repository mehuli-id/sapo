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
