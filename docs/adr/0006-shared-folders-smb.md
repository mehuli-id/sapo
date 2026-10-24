# ADR 0006: Shared folder, akun sistem, dan SMB

- **Status:** Diterima
- **Tanggal:** 2026-10-24

## Konteks

Pengguna dan grup Sapo (ADR 0003) baru ada di data panel. Untuk berbagi file lewat SMB, setiap pengguna butuh akun Linux (pemilik file) dan password Samba, folder butuh izin di sistem berkas, dan Samba butuh konfigurasi. Semua ini mengubah sistem, jadi dikerjakan `sapod` (ADR 0002), dan menyangkut siapa bisa membaca data siapa.

## Keputusan

- **Akun sistem dikelola deklaratif** (`accounts.sync`). `sapo-api` mengirim daftar lengkap pengguna dan grup; `sapod` membuat, mengubah anggota, dan menghapus akun dan grup Linux sampai sama.
  - `sapod` hanya menyentuh akun dan grup ber-UID/GID di rentang `agent.accounts.id_min`–`id_max` (bawaan 20000–29999), dan akun harus bergrup utama `primary_group` (`sapo-users`). Akun lain (root, akun paket, admin lokal) tidak pernah diubah.
  - Nama yang sudah dipakai akun atau grup di luar rentang dilaporkan sebagai bentrok dan tidak mendapat izin folder.
  - Akun tanpa shell dan tanpa home. Menghapus pengguna menghapus akunnya, tidak menghapus filenya.
- **Password Samba sama dengan password panel.** Hash NT disimpan Samba sendiri, disamakan saat password tersedia dalam bentuk asli: pengguna dibuat, password diganti atau di-reset, dan setiap login (untuk akun lama sebelum Samba terpasang). Password dikirim ke `smbpasswd` lewat stdin, tidak pernah di argumen. Pengguna nonaktif dinonaktifkan juga di Samba.
- **Shared folder adalah dataset ZFS** `<pool>/<nama>`. Definisi folder (pool, keterangan, izin) disimpan `sapo-api` di `shares.json`; `shares.apply` menerapkan keadaan lengkap.
  - Folder: pemilik root, mode 0770, ACL POSIX per pengguna atau grup (baca = `r-x`, baca & tulis = `rwx`), dengan ACL bawaan agar file baru mewarisi izin yang sama.
  - `shares.apply` tidak pernah menghapus folder. Menghapus folder beserta isinya hanya lewat `shares.delete`, dengan nama diketik ulang, password admin, dan log audit sebelum dijalankan (sama dengan operasi pool).
- **`smb.conf` ditulis utuh oleh `sapod`** dari templat tetap, diperiksa `testparm`, lalu Samba dimuat ulang. Bila `testparm` menolak, konfigurasi lama tetap.
  - SMB2 minimum, tanpa tamu (`map to guest = never`, `restrict anonymous = 2`), tanpa printer dan usershare.
  - Setiap folder: `valid users` dan `write list` dari izin; folder tanpa izin tidak dibagikan sama sekali (di Samba, `valid users` kosong berarti semua pengguna boleh masuk).
  - Nama folder, nama akun, path, dan keterangan divalidasi ketat (tanpa baris baru, kurung siku, `%`, `\`), jadi tidak bisa menambah baris atau bagian ke `smb.conf`.
- **Memberi akses butuh password admin**: membuat folder, mengubah izin folder, mengubah grup pengguna, dan mengubah anggota grup.
- **`sapo-api` menyamakan sistem saat menyala** dan setelah setiap perubahan pengguna, grup, atau folder. Bila gagal (misalnya `sapod` belum siap), perubahan di panel tetap tersimpan dan panel memberi peringatan.

## Alasan

- Keadaan lengkap yang dikirim ulang membuat sistem bisa diperbaiki sendiri (misalnya setelah restore atau restart) dan menghindari perubahan setengah jadi.
- Rentang UID/GID dan grup utama penanda memastikan `sapod` tidak bisa dipakai mengubah akun di luar Sapo, walau `sapo-api` disusupi.
- Izin ditegakkan dua lapis: Samba (`valid users`, `write list`) dan sistem berkas (ACL), jadi akses lain di masa depan (NFS, File Station) tidak otomatis terbuka.

## Konsekuensi

- Mengubah izin hanya memasang ACL di folder teratas dan ACL bawaannya. File yang sudah ada mempertahankan ACL-nya; izin berbasis Samba tetap berlaku segera. Menerapkan ulang izin ke isi folder yang besar menyusul sebagai tugas latar belakang.
- Pengguna yang belum pernah login sejak Samba terpasang belum punya password Samba sampai login sekali.
- NFS menyusul dengan izin dari data yang sama.
