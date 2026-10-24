# ADR 0005: Pool penyimpanan ZFS dari panel

- **Status:** Diterima
- **Tanggal:** 2026-10-24

## Konteks

Semua fitur data Fase 1 (shared folder, SMB/NFS, File Station, Sapo Drive, App Center) butuh tempat penyimpanan. Dokumen rancangan menyebut ZFS (mirror, RAID-Z) dan mdadm sebagai pilihan, dan menyisakan pertanyaan apakah Btrfs masuk Fase 1.

Membuat pool adalah operasi pertama `sapod` yang menghapus data. Kesalahan memilih disk tidak bisa dibatalkan, jadi prinsip "data dulu" paling diuji di sini.

## Keputusan

- **ZFS dulu.** mdadm menyusul sebagai alternatif untuk mesin ber-RAM kecil. Btrfs tidak masuk Fase 1: RAID 5/6 Btrfs masih tidak disarankan untuk data penting.
- **Susunan yang tersedia:** `single`, `mirror`, `raidz1`, `raidz2`, dan `raidz3`.
  - Artinya ditetapkan di kode (`sapod` dan panel).
  - `agent.storage.pools.layouts` hanya memilih susunan yang ditawarkan dan batas jumlah disknya. Batas itu tidak boleh di bawah minimum ZFS (`proto.LayoutFloor`).
  - Stripe tanpa redundansi lebih dari satu disk tidak ditawarkan.
- **Operasi `sapod`:**
  - `storage.pools` membaca `zpool list -H -p` dan `zpool status -j --json-int` (OpenZFS 2.3). Hasilnya: kesehatan pool, kapasitas, pohon vdev dengan disk kernelnya, galat, dan scrub atau resilver.
  - `storage.pool_create` membuat pool.
  - `storage.pool_scrub` memulai atau menghentikan scrub.
  - Ganti disk dan hapus pool menyusul dengan aturan konfirmasi yang sama.
- **`sapod` memeriksa ulang semuanya sebelum `zpool create`,** tanpa mempercayai panel atau `sapo-api`:
  - nama pool: huruf kecil, maksimal 32 karakter, bukan nama cadangan ZFS, dan belum ada;
  - susunan dan jumlah disk sesuai konfigurasi;
  - setiap disk dibaca ulang dari `lsblk` (bukan cache), dan nomor seri, WWN, serta ukurannya harus sama dengan yang dilihat pengguna (`disk_changed`);
  - disk yang dipakai ditolak (`disk_in_use`): sistem berkas terpasang (termasuk disk sistem), swap, anggota pool ZFS lain, RAID mdadm, LVM, atau volume terenkripsi yang terbuka;
  - disk lepas-pasang ditolak kecuali `allow_removable` (`disk_removable`);
  - disk yang masih punya tabel partisi atau tanda tangan sistem berkas (`data`) hanya dipakai bila penghapusannya dikonfirmasi per disk (`erase`, `disk_has_data`).
- **`zpool create -f`** hanya dipakai bila ada disk berisi data yang sudah dikonfirmasi, atau ukuran disk berbeda. Disk yang sedang dipakai sudah ditolak sebelumnya. Hanya satu operasi pool yang mengubah sistem berjalan pada satu waktu.
- **Pool dibuat dengan nama perangkat `/dev/disk/by-id`** (WWN dulu), agar tidak bergantung pada urutan `sdX`.
  - Pilihan bawaan: `ashift=12`, `compression=lz4`, `atime=off`, `xattr=sa`, `acltype=posixacl`.
  - Pool dipasang di `/srv/sapo/<nama>`.
  - Semua pilihan ini ada di `agent.storage.pools`.
- **`sapo-api` meminta tiga hal sebelum meneruskan ke `sapod`:**
  - password lagi, dengan batas percobaan `reauth:<user>` (ADR 0003);
  - nama pool diketik ulang;
  - konfirmasi penghapusan.

  Hanya administrator yang boleh. Permintaan dicatat di log audit sebelum disk disentuh, lalu hasilnya dicatat lagi. Bila log audit tidak bisa ditulis, pool tidak dibuat. `sapod` juga mencatat perintah dan perangkatnya di log-nya.
- **Wizard "Buat pool penyimpanan"** (`wizards/pool-create.yaml`) memakai jenis isian baru: `disk-select`, `pool-layout`, `erase-confirm`, `confirm`, dan `current-password`.
  - Panel menyarankan susunan menurut jumlah disk: 2 disk mirror, 3 disk RAID-Z1, 4 disk atau lebih RAID-Z2.
  - Panel memperkirakan kapasitas dan jumlah disk yang boleh rusak.
  - Panel mengingatkan bahwa RAID bukan cadangan.

## Alasan

- ZFS memberi checksum per blok, scrub, snapshot, dan boot environment (rollback update) dalam satu sistem. Ini sejalan dengan prinsip data dulu.
- Pemeriksaan identitas disk mencegah kasus paling berbahaya: disk dicabut atau ditukar antara saat dipilih dan saat dihapus, atau nama `sdX` bergeser setelah reboot.
- Kata sandi dan nama yang diketik ulang membuat sesi yang dicuri atau klik yang tidak sengaja tidak cukup untuk menghapus disk.

## Alternatif

- **Btrfs di Fase 1.** Ditunda. RAID 5/6 belum stabil, dan dua sistem berkas berarti dua jalur uji untuk operasi yang paling berisiko.
- **Memakai `-f` selalu.** Ditolak. Pemeriksaan `zpool` sendiri tetap menjadi lapisan pengaman kedua untuk disk yang belum dikonfirmasi.
- **Membiarkan panel menilai disk mana yang boleh.** Ditolak. Penilaian itu ada di `sapod` (`Disk.usage`). Panel hanya menampilkannya.

## Konsekuensi

- ZFS butuh `zfs-dkms` (contrib, CDDL), jadi modul dikompilasi di mesin pengguna (ADR 0001). ISO dev memasangnya dan memeriksa modulnya saat build. Uji asap QEMU membuat pool mirror sungguhan di disk virtual lewat API, lalu menjalankan scrub.
- Disk dari pool ZFS lain (misalnya NAS lama) belum bisa dipakai atau diimpor dari panel. Itu menunggu fitur impor pool.
- `storage.pools` bergantung pada format JSON OpenZFS 2.3 (Debian trixie). Bila formatnya berubah, daftar pool dan kapasitas tetap tampil dari `zpool list`.
