# ADR 0015: Penyimpanan eksternal (USB)

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

Pengguna ingin membuka flashdisk atau HDD eksternal (termasuk dock 2-bay) dari NAS. Format yang lazim di drive itu (exFAT, FAT32, NTFS) tidak menyimpan pemilik dan izin Linux, jadi izin harus ditentukan saat drive dipasang. Disk USB juga kurang andal untuk pool ZFS, dan banyak enclosure murah melaporkan nomor seri yang sama (nomor seri chip USB) untuk beberapa disk.

## Keputusan

- **Daftar drive** (`external.list`, Storage Manager):
  - semua sistem berkas di disk bertransport `usb` (disk tanpa partisi atau tiap partisi), dikelompokkan per perangkat;
  - setiap entri menampilkan format, label, ukuran, dan alasan bila tidak bisa dipasang: format lain, anggota pool/RAID/LVM/terenkripsi/swap, tanpa UUID, atau sedang dipakai sistem.
- **Pasang sebagai folder eksternal** (`external.mount`, admin, password, log audit):
  - nama folder (aturan sama dengan shared folder, tidak boleh bentrok), satu grup Sapo yang boleh membuka (`sapo-users` = semua pengguna), baca saja atau baca-tulis untuk grup itu, opsional SMB;
  - `sapod` memasang di `agent.external.mount_base/<nama>` dengan opsi tetap: `uid=0,gid=<grup>`, `fmask/dmask` (grup rw atau r, lainnya tanpa akses), `ro` bila baca saja, selalu `nosuid,nodev,noexec`;
  - hanya exFAT, FAT (`vfat`, dengan `flush`), dan NTFS (`ntfs3` dari kernel);
  - Sapo tidak pernah memformat atau mengubah isi drive.
- **Diingat per sidik jari drive** (`agent.external.state_file`): UUID sistem berkas, format, ukuran, dan nomor seri disk, karena UUID FAT/exFAT hanya 32 bit dan ikut tersalin saat drive dikloning. Juga dicatat GID grup saat dipasang. `sapod` memeriksa setiap 10 detik:
  - folder yang diatur dan drivenya tercolok dipasang lagi (setelah reboot atau dicolok ulang), hanya bila sidik jarinya sama dan GID grupnya masih sama;
  - drive lain dengan UUID sama tidak dipasang otomatis (dilepas bila sedang terpasang), dan UUID yang dipakai lebih dari satu volume tidak pernah dipasang;
  - folder yang drivenya tidak terlihat dalam 3 pemeriksaan berturut-turut (bukan sekadar USB direset sesaat) dilepas paksa (`umount -l`, drive sudah tidak ada) dan tampil "tidak tercolok";
  - pemasangan yang gagal dicoba lagi dengan jeda yang makin panjang (sampai 10 menit) dan dicatat di log.
- **Setelah dipasang**, akar drive harus milik root, grup itu, tanpa akses untuk lainnya. Bila tidak (misalnya atribut khusus di drive NTFS), drive dilepas lagi.
- **Partisi sistem** (EFI, Microsoft Reserved, Windows Recovery) tidak ditawarkan.
- **Akses**:
  - **File Station dan WebDAV:** folder eksternal tampil bagi anggota grupnya. `FilesTarget.External` membuat `sapod` hanya memakai folder yang sedang terpasang olehnya sebagai akar, dan operasi tetap sebagai akun pengguna (kernel menegakkan izin grup dari opsi mount). Tidak ada snapshot.
  - **SMB:** bagian per folder (`valid users = +<grup>`, `read only` sesuai pilihan, tanpa `shadow_copy2`). `smb.conf` ditulis ulang saat drive dipasang atau dilepas, memakai masukan `shares.apply` terakhir. Saat sinkronisasi akun gagal-tertutup, folder eksternal ikut tidak dibagikan (`ExternalOff`). Sebelum ada `shares.apply` pertama sejak `sapod` menyala, `smb.conf` tidak ditulis ulang; sinkronisasi berikutnya menyertakannya.
  - Daftar folder terpasang untuk File Station dibaca dari cache yang tidak pernah menunggu `lsblk`, `mount`, atau drive yang lambat.
- **Lepas dengan aman** (`external.unmount`):
  - koneksi SMB ke folder itu ditutup, lalu `umount` biasa, sehingga data ditulis tuntas;
  - bila masih dipakai, ditolak tanpa perubahan;
  - bila berhasil, konfigurasinya dilupakan.
- **Pool**:
  - disk USB tidak bisa dipilih untuk pool (buat atau ganti disk) kecuali admin mencentang "izinkan disk USB", dengan peringatan bahwa USB tidak cocok untuk penyimpanan utama;
  - disk yang nomor seri atau WWN-nya sama dengan disk lain, disk USB tanpa nomor seri dan WWN, dan beberapa disk tanpa identitas berukuran sama ditolak untuk operasi destruktif (`disk_identity`), karena tidak bisa dipastikan.
- **Dock dua disk**:
  - mode JBOD: dua drive, masing-masing bisa dipasang sendiri, atau dijadikan pool cadangan bila identitasnya unik;
  - mode RAID hardware: satu drive; kesehatan disk di dalamnya tidak terlihat.

## Konsekuensi

- Izin berlaku untuk seluruh drive (satu grup, satu mode). Izin per pengguna dalam satu drive membutuhkan dua grup per drive (belum).
- Format Linux (ext4, xfs, btrfs) belum bisa dipasang: izin di disk itu milik komputer lain, dan Sapo tidak mengubah kepemilikan file di disk orang.
- Format drive dan backup shared folder ke USB (pool cadangan) dikerjakan di tahap berikutnya.
