# ADR 0022: Time Machine untuk Mac

- **Status:** Diterima
- **Tanggal:** 2026-10-26

## Konteks

Banyak rumah dan kantor kecil memakai Mac. Time Machine adalah cara backup bawaan macOS, dan bisa memakai share SMB di jaringan bila server mengumumkannya sebagai tujuan Time Machine. Tanpa batas ukuran, Time Machine mengisi seluruh pool.

## Keputusan

- **Per shared folder**: admin menjadikan folder tujuan Time Machine dengan **kuota wajib** (GB, minimal 10), dari Shared Folder di panel. Butuh password admin dan tercatat di audit (`shares.time_machine`).
- **Kuota ZFS** `refquota` (snapshot tidak dihitung). Mac melihatnya sebagai ukuran disk, sehingga Time Machine membuang backup lama sendiri saat penuh. Samba juga diberi `fruit:time machine max size` dengan nilai yang sama.
  - `sapod` mencatat kuota yang dipasangnya di properti `sapo:tm`, dan hanya kuota itu yang dilepas saat Time Machine dimatikan. Kuota folder lain tidak pernah disentuh.
  - Kuota yang lebih kecil dari isi folder ditolak ZFS dan tampil sebagai galat di panel; folder tetap dibagikan dengan kuota lama.
  - `xattr=sa` dipasang di dataset itu (metadata Mac di dnode, lebih cepat).
- **Samba**:
  - Hanya folder Time Machine yang memuat `catia fruit streams_xattr` (bersama `shadow_copy2`), dengan `fruit:time machine = yes`.
  - Folder lain tidak berubah, karena `vfs_fruit` mengubah cara metadata disimpan.
  - Pengaturan `fruit:*` umum ada di `[global]` dan hanya berlaku untuk share yang memuat `fruit`.
  - Handle tahan putus (`durable handles`, tanpa kernel oplocks/share modes/posix locking) agar backup lewat Wi-Fi tidak rusak saat koneksi tersendat.
- **Pengumuman di jaringan**: Samba sendiri mendaftarkan `_adisk._tcp` lewat Avahi (`multicast dns register = yes`; Avahi sudah ada di fitur inti), sehingga folder muncul di Pengaturan Time Machine Mac. Tidak ada file layanan Avahi tambahan.
- **Tempat sampah (ADR 0021) selalu mati** di folder Time Machine, karena Time Machine menghapus banyak file kecil setiap backup.
  - Menyalakan Time Machine mematikan tempat sampah folder itu, dan menyalakan tempat sampah di folder Time Machine ditolak (`time_machine_share`).
  - `sapod` juga tidak pernah memasang `vfs_recycle` di folder Time Machine.
- **Akses**: Mac masuk dengan akun Sapo yang punya izin tulis ke folder itu (izin folder biasa). Beberapa Mac bisa memakai satu folder; kuota berlaku untuk semuanya.
- **Paket**: `samba-vfs-modules` (modul `fruit`, `catia`, `streams_xattr`, `recycle`) ikut fitur `smb`.

## Akibat

- Mematikan Time Machine tidak menghapus backup Mac yang ada; kuotanya dilepas dan folder kembali biasa.
- Kuota per Mac dan pengumuman per pengguna belum ada.
- Uji asap ISO: kuota terlalu kecil ditolak, folder tujuan dibuka dan ditulis lewat SMB (smbd menolak masuk bila modul VFS tidak ada), tempat sampah mati dan tidak bisa dinyalakan, lalu dimatikan lagi. Pengumuman Avahi dan backup dari Mac sungguhan diuji manual.
