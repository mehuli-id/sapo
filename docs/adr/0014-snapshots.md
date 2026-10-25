# ADR 0014: Snapshot ZFS terjadwal

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

Data dulu: file yang terhapus atau tertimpa (salah klik, ransomware di komputer klien) harus bisa dikembalikan. ZFS menyediakan snapshot yang murah dan seketika. Admin harus bisa menyalakan, mematikan, dan mengatur jadwalnya per folder dari panel.

## Keputusan

- **Jadwal per dataset**:
  - jumlah snapshot per jam, harian, dan mingguan (Senin) yang disimpan, dan jam snapshot harian/mingguan diambil (waktu lokal NAS, mengikuti zona waktu di Control Panel);
  - 0 berarti jenis itu tidak diambil;
  - berlaku untuk setiap shared folder sendiri-sendiri, dan satu jadwal untuk semua folder pribadi Sapo Drive;
  - templat siap pakai ada di `api.snapshot_presets` (`sapo.yaml`), dan admin juga bisa mengisi angka sendiri.
- **Jadwal disimpan di dataset itu sendiri** (properti ZFS `sapo:snap`, misalnya `h24.d14.w8.t2`). Properti ini diatur oleh `sapod` lewat sinkronisasi (`shares.apply`, `homes.apply`); sapo-api menyimpannya di `shares.json` dan `settings.json`. Jadwal ikut terbawa bila pool diimpor ke NAS lain. Saat sinkronisasi gagal-tertutup (izin dicabut semua), jadwal snapshot tetap dikirim.
- **`sapod` memeriksa jadwal setiap menit**:
  - hanya dataset buatan Sapo, yaitu `<pool>/<folder>` bertanda `sapo:share` dan `<pool>/sapo-home/<nama>` bertanda `sapo:home`;
  - mengambil snapshot bila periode berjalan (jam ini, hari ini sejak jam yang dipilih, minggu ini sejak Senin) belum punya snapshot jenis itu, jadi NAS yang mati saat jadwal tiba mengejar saat menyala;
  - membuang snapshot otomatis yang melebihi jumlah simpan (satu perintah per jenis, dicatat di log);
  - kunci pool hanya diambil per dataset, dan setiap perintah `zfs` dibatasi `agent.snapshots.call_timeout_seconds`; dataset yang pool-nya sedang dipakai operasi lain dilewati sampai menit berikutnya;
  - pool yang tidak ONLINE/DEGRADED dilewati;
  - di pool yang terisi `agent.snapshots.max_pool_capacity_percent` (bawaan 90%) atau lebih, snapshot baru tidak diambil (snapshot lama tetap dibuang sesuai jadwal), agar snapshot tidak membuat pool penuh;
  - snapshot bertanggal lebih dari 1 jam di masa depan (jam sistem pernah salah) tidak dihitung dan tidak dibuang otomatis, jadi jadwal tetap berjalan;
  - kegagalan dicatat di log, paling sering sekali per jam untuk hal yang sama.
- **Nama snapshot** `sapo-<hourly|daily|weekly|manual>_GMT-YYYY.MM.DD-HH.MM.SS` (UTC). Snapshot buatan Sapo juga diberi properti `sapo:auto=<jenis>`. Hanya snapshot bernama seperti ini **dan** bertanda yang dibuang otomatis; snapshot buatan admin dari terminal (walau bernama mirip) tidak pernah disentuh.
- **Data dulu**:
  - mematikan jadwal atau satu jenisnya tidak menghapus snapshot yang sudah ada;
  - snapshot manual tidak pernah dihapus otomatis;
  - menghapus satu snapshot butuh password admin dan tercatat di log audit, tanpa `-r`;
  - jadwal baru yang akan membuang snapshot yang ada (jumlah simpan diturunkan di bawah jumlah snapshot yang ada, atau jenis baru untuk folder pribadi) juga butuh password admin;
  - jadwal yang gagal diatur tidak membuat folder berhenti dibagikan (dilaporkan terpisah);
  - kuota Sapo Drive memakai `refquota`, jadi snapshot tidak memakan batas pengguna;
  - panel menampilkan ruang yang hanya dipakai snapshot.
- **"Previous Versions" di Windows**: Samba `vfs_shadow_copy2` (`.zfs/snapshot`, format nama di atas) untuk semua shared folder dan folder pribadi.
- **Operasi** (admin, log audit): `snapshots.list`, `snapshots.create` (manual), `snapshots.delete`, jadwal per folder dan untuk folder pribadi.

## Konsekuensi

- Snapshot berada di pool yang sama: bukan pengganti backup ke disk lain (menyusul bersama penyimpanan eksternal).
- Memulihkan file dari panel (melihat versi lama di File Station, menyalin kembali) dikerjakan di tahap berikutnya; untuk sekarang lewat "Previous Versions" di Windows.
- Snapshot yang terus ditahan di pool yang hampir penuh memakan ruang; admin melihat pemakaiannya per folder dan bisa mengurangi jumlah simpan. Karena kuota Sapo Drive memakai `refquota`, pengguna yang terus menulis dan menghapus data besar bisa memakai pool lebih dari kuotanya lewat snapshot; batas kapasitas pool di atas menghentikan snapshot baru sebelum pool penuh.
- Snapshot menyimpan file dengan izin saat snapshot diambil. Pengguna yang masih punya akses ke folder bisa membuka versi lama lewat Previous Versions (SMB) atau `.zfs/snapshot` (NFS), termasuk file yang kemudian dibatasi atau dihapus, sampai snapshot itu habis masa simpannya atau dihapus admin.
- Jadwal di dataset yang tidak lagi dikenal panel (Sapo Drive dimatikan, folder pribadi pengguna yang dihapus) tetap berjalan sampai datanya dihapus. Datanya aman, karena yang dibuang hanya snapshot otomatis.
