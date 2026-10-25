# ADR 0017: Backup shared folder ke pool lain

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

Snapshot (ADR 0014) melindungi dari file yang terhapus atau berubah, tetapi berada di pool yang sama: bila semua disk pool rusak, snapshot ikut hilang. Pengguna butuh salinan di disk lain, misalnya drive USB atau dock 2-bay yang dijadikan pool (ADR 0015, `allow_usb`), diatur sepenuhnya dari panel.

## Keputusan

- **Tugas backup** = satu shared folder → satu pool tujuan lain, jadwal harian (jam, waktu lokal NAS), jumlah salinan disimpan (1–366), nyala/mati. Dibuat lewat wizard `wizards/backup-create.yaml` di aplikasi **Backup**; isian `share-select` baru, dan `pool-select` dengan `source` tidak menawarkan pool folder itu sendiri. Tugas dan hasil terakhir disimpan `sapod` di `agent.backup.state_file`.
- **ZFS send/receive.** Backup pertama `zfs send -L -c` penuh, berikutnya `-i` dari snapshot backup terakhir yang ada di kedua sisi. `sapod` menjalankan dua proses `zfs` bersambung lewat pipa (tanpa shell). Snapshot di sumber bernama `sapo-backup-<id>_GMT-<waktu>` dan hanya yang terbaru disimpan (dasar backup berikutnya); nama ini tidak cocok dengan pola snapshot terjadwal, jadi tidak dipangkas penjadwal snapshot dan tidak tampil sebagai versi lama.
- **Di tujuan** data diterima di `<pool>/sapo-backup/<id>` dengan `canmount=noauto`, `readonly=on`, dan penanda `sapo:backup=<id>` (induk `sapo-backup` bertanda `@induk`, `canmount=off`). Tidak ada penanda `sapo:share`, jadi tidak pernah dibagikan lewat SMB/NFS/File Station. Dataset bernama sama yang bukan milik tugas itu tidak pernah disentuh.
- **Data dulu.**
  - Backup tidak pernah mengubah atau menghapus data di folder asal; yang dibuang hanya snapshot backup lama.
  - `zfs receive -F` tidak dipakai. Bila salinan di tujuan tidak punya dasar bersama dengan sumber (`backup_diverged`), backup berhenti dan admin harus menghapus salinan lama secara eksplisit.
  - Snapshot yang gagal dikirim dibuang dari sumber; salinan lama di tujuan tetap.
  - Folder yang di-backup dan pool sumber/tujuan tugas tidak bisa dihapus selama tugas ada (`sapod` menolak, `409`).
  - Mengubah tugas, menghapus tugas, dan memulihkan butuh password admin dan tercatat di log audit. Menghapus beserta salinan butuh mengetik ulang ID tugas (`destructive`).
- **Pemulihan** selalu membuat shared folder **baru** di pool asal (`zfs send` salinan → `zfs receive` dengan `sapo:share=<nama baru>`); folder yang ada tidak pernah ditimpa. Setelah selesai, sapo-api mendaftarkan folder itu dengan izin folder asalnya. Pendaftaran hanya untuk pemulihan yang datasetnya masih ada (`present`), sehingga folder yang kemudian dihapus admin tidak pernah didaftarkan ulang (dan tidak dibuat kosong oleh sinkronisasi).
- **Satu backup atau pemulihan pada satu waktu**, paling lama `agent.backup.max_run_hours`. Pembuatan dan pemangkasan snapshot memegang kunci pool sebentar; pengiriman data berjalan tanpa kunci pool. Pool tujuan yang hilang (drive dicabut) membuat backup gagal dengan pesan jelas dan dicoba lagi paling cepat sejam kemudian.

## Akibat

- Backup tahap ini hanya ke pool lokal (internal atau USB). Backup ke NAS lain atau cloud (rsync/SSH, S3) menyusul dengan tipe tujuan baru.
- Tujuan berupa pool ZFS, jadi drive USB harus dijadikan pool (dihapus isinya). Drive exFAT/NTFS yang dipasang sebagai folder eksternal tidak dipakai untuk backup.
- Salinan menyimpan riwayat sesuai jumlah simpan; ruang di tujuan dipakai hanya untuk bagian yang berubah.
- Uji asap ISO membuat pool cadangan, menjalankan backup penuh dan inkremental, memastikan penolakan hapus pool/folder, memulihkan salinan pertama sebagai folder baru, dan membacanya lewat SMB.
