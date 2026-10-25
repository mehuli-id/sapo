# ADR 0016: Installer ke disk

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

ISO Sapo berjalan dari RAM: semua perubahan hilang saat NAS dimatikan. Pengguna harus bisa memasang Sapo ke disk sistem dari panel (prinsip "semua lewat web"), tanpa internet, dan tanpa mempertaruhkan data di disk pool.

## Keputusan

- **Wizard "Pasang Sapo ke disk"** (`wizards/install.yaml`, Storage Manager) tampil hanya saat berjalan dari ISO (`agent.install.live_marker`).
  - Admin memilih satu disk sistem, dengan pemeriksaan yang sama dengan disk pool baru (`Pools.checkDisks`): identitas sama dengan yang dipilih, tidak dipakai pool/RAID/sistem, bukan USB atau disk lepas-pasang, penghapusan dikonfirmasi, nomor seri tidak kembar.
  - Ukuran disk minimal `min_disk_gb`.
  - Nama disk diketik ulang, password admin, dan log audit (`system.install`, lewat `destructive`).
- **Pemasangan berjalan di unit systemd tersendiri** (`sapod install-run`, seperti pembaruan sistem). Setiap langkah tercatat di log, dan panel menampilkan kemajuannya. Langkah-langkahnya:
  1. Partisi GPT: BIOS boot 1 MB, EFI 512 MB, root ext4 (sisanya).
  2. Sistem disalin dari squashfs ISO (lapisan bawah overlay live, persis yang diuji, tanpa internet).
  3. Pengaturan sesi live di `copy_paths` disalin: akun Linux, pengaturan dan data sapo-api/sapod, Samba, jaringan, nama host, zona waktu, `zpool.cache`, `hostid`.
  4. Berkas khusus ISO di `remove_paths` dibuang; akun live (`remove_users`) dan paket live (`purge_packages`) dihapus.
  5. `fstab` per UUID; machine-id baru.
  6. initramfs; GRUB untuk BIOS (`i386-pc`) dan UEFI (jalur removable, tanpa NVRAM); hook agar kernel baru dari pembaruan sistem masuk menu GRUB.
  7. Semua titik pasang dilepas.

  Bila satu langkah gagal, pemasangan berhenti. Disk pool tidak pernah disentuh.
- **Pengaman**:
  - `install-run` memeriksa ulang identitas disk (nomor seri, WWN, ukuran, tidak dipakai) dari `lsblk` tepat sebelum menghapusnya, hanya berjalan dari ISO, dan file parameternya sekali pakai (dihapus begitu dibaca, juga bila unit gagal dimulai);
  - disk media ISO ditolak menurut namanya (sumber mount `live_marker`);
  - pemasangan ditolak bila titik pasang tujuan masih terpasang dari percobaan sebelumnya;
  - selama pemasangan berjalan, tidak ada disk yang dihapus untuk pool (buat atau ganti disk);
  - SIGTERM (batas waktu unit) menghentikan perintah yang berjalan, dan titik pasang tetap dilepas (lazy bila perlu);
  - akun live (`remove_users`, password umum dan sudo) gagal-tertutup: pemasangan gagal bila akun itu masih ada di passwd, shadow, group, atau gshadow sistem baru;
  - `copy_paths` disamakan dengan sesi live (yang tidak ada di sana juga dibuang di sistem baru, misalnya NTP mati);
  - path tujuan tidak pernah melewati symlink keluar dari sistem baru;
  - sapo-api dan sapod tetap berjalan saat pengaturan disalin (sesi panel di memori tidak hilang). File datanya ditulis atomik, jadi setiap file tersalin utuh; log audit bisa terpotong di baris terakhir.
- **Root ext4 di satu disk**, bukan ZFS-on-root. Data tetap di pool ZFS terpisah. Disk sistem rusak = pasang ulang ke disk baru; pool diimpor lagi.
- **Setelah selesai**, panel meminta media ISO dicabut lalu NAS dinyalakan ulang. Sistem terpasang memakai akun, sertifikat HTTPS, pengaturan, dan pool yang sama, dan diperbarui lewat APT seperti biasa (ADR 0011).

## Konsekuensi

- Hanya amd64 (GRUB `i386-pc` dan `x86_64-efi`); arm64 menyusul. Secure Boot belum didukung.
- Disk sistem tanpa redundansi. Mirror atau ZFS-on-root untuk sistem bisa ditambahkan nanti.
- Perubahan setelah pemasangan (sebelum restart) tetap hanya di RAM.
- ISO dev juga memasang menu darurat di port serial ke sistem terpasang (ISO dev adalah image pengembangan).
- GRUB tanpa password: siapa pun di konsol fisik bisa mengubah baris perintah kernel (setara akses fisik).
- Paket yang diperbarui selama sesi live (di RAM) tidak ikut terpasang; sistem terpasang memakai paket dari ISO lalu diperbarui lewat APT.
