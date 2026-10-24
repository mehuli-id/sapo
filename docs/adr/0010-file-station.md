# ADR 0010: File Station

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

Pengguna ingin membuka, mengunggah, dan mengatur file di shared folder dari browser, tanpa SMB atau NFS. Izinnya harus persis sama dengan SMB (ADR 0006). `sapo-api` berjalan sebagai user `sapo` tanpa akses ke isi folder, dan `sapod` (root) tidak boleh menjadi jalan pintas melewati izin.

## Keputusan

- **Operasi file dijalankan sebagai akun Linux pengguna yang login.** Untuk setiap operasi, `sapod` menjalankan dirinya sendiri sebagai proses pembantu (`sapod fs-helper`) dengan UID, GID, dan grup tambahan akun itu (setuid/setgid/setgroups sebelum program berjalan; pembantu menolak berjalan sebagai root). Kernel yang menegakkan ACL folder, jadi hasilnya sama dengan SMB, termasuk file baru yang dimiliki pengguna dan mewarisi ACL folder.
  - Hanya akun buatan `sapod` (catatan akun, ADR 0006); grup tambahan hanya grup di rentang ID Sapo (bukan grup sistem seperti `sudo` atau `disk`).
  - Hanya shared folder Sapo yang terpasang (penanda `sapo:share`, mountpoint dan status terpasang diperiksa, seperti ADR 0006).
- **Path dibuka dengan `openat2`** relatif terhadap akar folder dengan `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS | RESOLVE_NO_XDEV`: tidak bisa keluar dari folder, tidak mengikuti symlink (termasuk nama terakhir), tidak masuk sistem berkas lain (snapshot `.zfs`, mount lain). Panel dan `sapod` juga menolak `..`, `.zfs`, karakter kendali, dan nama lebih dari 255 byte. Symlink dan file khusus tampil tetapi tidak bisa dibuka.
- **Isi file mengalir lewat file descriptor**: pembantu membuka file (sebagai pengguna), mengirim descriptor-nya ke `sapod` lewat socketpair, lalu `sapod` meneruskannya ke `sapo-api` (SCM_RIGHTS di socket `sapod`). `sapo-api` membaca atau menulis langsung; data tidak melewati pesan JSON (batas 1 MiB).
  - Unduhan selalu `Content-Disposition: attachment`, `application/octet-stream`, dan `Content-Security-Policy: sandbox`: file HTML atau SVG pengguna tidak pernah berjalan di origin panel.
  - Unggahan ditulis ke file sementara `.sapo-upload-<acak>` (tidak tampil, tidak bisa dipakai sebagai nama), lalu diberi nama akhir dengan `renameat2(RENAME_NOREPLACE)`. Menimpa file yang ada hanya bila pengguna setuju, hanya file yang boleh ditulis pengguna (bukan sekadar direktorinya), dan tidak pernah folder atau symlink. Unggahan yang terputus dibuang; file sementara yang tertinggal lebih dari sehari dibersihkan saat unggahan berikutnya di folder itu.
- **Operasi**: daftar isi (paling banyak `agent.files.max_entries`, dan tetap muat di satu pesan `sapod`; nama yang bukan UTF-8 tampil tetapi tidak bisa dipakai), unduh, unggah, buat folder, ganti nama/pindah (tidak menimpa), dan hapus (folder beserta isinya, tanpa mengikuti symlink, dengan konfirmasi di panel).
- **Semua pengguna** bisa memakai File Station; panel hanya menampilkan folder dengan izin efektif pengguna itu (aturan ACL POSIX: izin langsung dulu, lalu grup), dan menolak perubahan di folder baca-saja sebelum sampai ke `sapod`. Unggah (termasuk yang gagal), buat folder, ganti nama, dan hapus tercatat di log audit; hasil hapus dicatat setelah `sapod` selesai.
- **Batas**: paling banyak 8 operasi file bersamaan dan satu penghapusan per pengguna, agar File Station tidak menghabiskan koneksi `sapod` untuk operasi lain. Selama data mengalir, batas waktu HTTP diperpanjang (unggahan dan unduhan besar).

## Alasan

- Menegakkan izin lewat kernel menghindari menulis ulang logika ACL di `sapod` (yang bisa salah dan menjadi jalan pintas root).
- Proses terpisah per operasi lebih sederhana dan lebih aman daripada mengganti identitas thread di proses root Go.

## Konsekuensi

- `sapo-api` dipercaya menyebut pengguna yang login. `sapo-api` yang disusupi bisa membuka file pengguna Sapo mana pun (tidak pernah file root atau akun sistem). Itu setara dengan kemampuannya mengganti password pengguna (ADR 0003, 0006).
- Satu proses per operasi: cukup untuk penggunaan di panel, bukan untuk ribuan operasi per detik.
- Menghapus folder yang sangat besar dibatasi waktu (`delete_timeout_seconds`); bila lewat, sebagian sudah terhapus.
- Unggahan yang sangat lambat tetap ditunggu selama data terus datang (tanpa kecepatan minimum).
- Unduh folder sebagai zip, pratinjau, dan pencarian belum ada.
