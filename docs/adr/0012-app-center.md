# ADR 0012: App Center

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

Pengguna NAS ingin memasang aplikasi tambahan (dasbor, pemantau layanan, media server, foto) dari panel. Container adalah cara umum membagikan aplikasi seperti itu, tetapi container yang dijalankan tanpa batas setara dengan akses root ke NAS dan semua data. Pilihan yang disepakati: **Podman dari Debian** (tanpa daemon root yang selalu berjalan) dan **katalog kurasi saja** (tanpa compose atau image bebas dari panel).

## Keputusan

- **Katalog adalah data**: `apps/<id>.yaml` di repo (dipasang ke `/usr/share/sapo/apps`). Setiap aplikasi: nama dan keterangan (id/en), image yang **dikunci digest** (`repo:tag@sha256:…`), port web di dalam container, port bawaan di NAS, path data di dalam container, variabel lingkungan tetap, batas memori. Admin hanya memilih aplikasi, pool tempat datanya, dan port.
- **Setiap aplikasi berjalan sebagai akun Linux sendiri** (`sapo-app-<id>`, UID/GID dari rentang tersendiri `agent.apps.id_min`–`id_max`, dicatat di `agent.apps.state_file`, tidak pernah dipakai ulang). Container dijalankan rootful Podman dengan `User=`/`Group=` akun itu, jadi proses di container adalah UID itu di host: tidak pernah root, tidak punya akses ke shared folder kecuali yang diberikan admin (lihat tahap 2), dan hanya bisa menulis datanya sendiri.
- **Pembatasan tetap** di setiap container, tidak bisa diubah dari katalog: semua capability dicabut (`DropCapability=all`), `NoNewPrivileges=true`, tidak bisa membuka port di bawah 1024, tanpa `--privileged`, tanpa jaringan host, tanpa device, tanpa socket Podman/Docker, batas memori dan jumlah proses. Hanya port web yang dibuka di NAS; port yang sedang dipakai program lain atau dicadangkan (panel, SMB, NFS, …) ditolak.
- **Jaringan container (`agent.apps.subnet`, bawaan Podman 10.88.0.0/16) tidak dianggap LAN**: ditolak panel (`api.denied_networks`), Samba (`hosts deny`), dan tidak bisa menjadi klien NFS. Aplikasi yang disusupi tidak mendapat akses lebih dari komputer di internet.
- **Data aplikasi di dataset ZFS** `<pool>/sapo-apps/<id>` (penanda `sapo:app=<id>`, pemilik akun aplikasi, mode 0700), terpasang di `<mount_base>/<pool>/sapo-apps/<id>`. Nama shared folder `sapo-apps` dicadangkan.
- **Dijalankan systemd lewat Quadlet**: `sapod` menulis `/etc/containers/systemd/sapo-app-<id>.container` dari templat tetap, lalu `systemctl daemon-reload` dan `start`. Aplikasi menyala lagi saat boot bila tidak dihentikan admin; yang terus gagal berhenti dicoba. Image diunduh saat pertama dijalankan. Saat `sapod` menyala, file Quadlet disamakan dengan katalog (digest baru dari paket Sapo) dan aplikasi yang berubah di-restart.
- **Pool yang masih berisi data aplikasi tidak bisa dihapus** sampai aplikasinya dihapus beserta data.
- **Operasi** (`sapod`, admin, log audit): katalog, daftar terpasang dengan status, pasang (password admin), jalankan, hentikan, hapus. Menghapus menyimpan data kecuali admin memilih menghapus data juga (nama diketik ulang, password admin, dataset dihapus).
- Nama pengguna panel yang berawalan `sapo-` ditolak, agar tidak bentrok dengan akun aplikasi.

## Alasan

- Akun per aplikasi membuat kernel yang membatasi akses, sama seperti File Station (ADR 0010); tahap 2 (akses shared folder) cukup menambah entri ACL untuk akun aplikasi.
- Quadlet membuat container menjadi layanan systemd biasa (restart, boot, log journal) tanpa daemon tambahan.
- Digest mengunci isi image: katalog yang diuji adalah yang dipasang.

## Konsekuensi

- Aplikasi yang butuh root, capability, atau port di bawah 1024 tidak masuk katalog (atau dikonfigurasi ulang lewat variabel lingkungan).
- Antarmuka web aplikasi lewat HTTP di port sendiri (bukan lewat HTTPS panel); aplikasi yang wajib HTTPS menunggu reverse proxy (tahap berikutnya).
- Port aplikasi terbuka di semua antarmuka jaringan NAS, tidak dibatasi `api.allowed_networks` (itu hanya untuk panel). Aplikasi yang akun admin-nya dibuat oleh pengunjung pertama (misalnya Uptime Kuma) sebaiknya langsung disiapkan setelah dipasang. Pembatasan lewat firewall menyusul.
- Pembaruan aplikasi = pembaruan katalog (digest baru) lewat paket Sapo.

## Tahap 2: akses shared folder (2026-10-25)

- **Katalog** menyatakan `share_path` (misalnya `/media` untuk Jellyfin) dan `share_write` (boleh diberi izin tulis; bawaan tidak). Aplikasi tanpa `share_path` tidak bisa diberi folder.
- **Admin memilih** di App Center folder mana yang boleh dibuka aplikasi, baca saja atau baca-tulis (password admin, log audit `apps.folders`). Pilihan disimpan sapo-api bersama izin folder (`shares.json`, kolom `apps`) dan dikirim ke `sapod` sebagai keadaan lengkap lewat `shares.apply`, sama seperti izin pengguna dan ekspor NFS.
- **Dua lapis penegakan.** (1) Akun aplikasi mendapat entri ACL sendiri di folder dan isinya (baca = `r-x`, tulis = `rwx`), diterapkan oleh mekanisme ACL yang sama dengan SMB, jadi kernel yang membatasi; Samba tidak memakai entri itu. (2) Hanya folder yang diberikan yang dipasang di container (`Volume=<folder>:<share_path>/<nama>:ro,nosuid,nodev`, `rw` hanya bila diberi izin tulis dan katalog mengizinkan). `<share_path>` sendiri tmpfs kecil milik root, jadi aplikasi tidak bisa membuat apa pun di sana.
- **Perubahan langsung berlaku**: file Quadlet aplikasi yang folder-nya berubah ditulis ulang dan aplikasi yang sedang berjalan di-restart. Izin untuk file yang sudah ada diterapkan di latar belakang, jadi file lama di folder besar bisa terlihat beberapa saat kemudian. Pencabutan melepas folder dari container saat restart itu.
- **Pengaman**: folder yang diberikan ke aplikasi tidak bisa dihapus (panel dan `sapod`) dan pool yang berisi folder itu tidak bisa dihapus sampai folder dilepas; aplikasi yang dihapus kehilangan semua akses foldernya. Bila akun pengguna gagal disinkronkan (gagal-tertutup), akses aplikasi juga dicabut. ID aplikasi paling panjang 23 karakter agar nama akun `sapo-app-<id>` muat batas 32 karakter.
- **Katalog** bertambah Jellyfin (media, baca saja) dan miniserve (menampilkan folder lewat browser, baca saja, tanpa login; dipakai juga di uji asap ISO).
- **Urutan saat dicabut atau diturunkan**: aplikasi yang akan kehilangan folder atau izin tulis dihentikan (sinkron) sebelum ACL folder diubah, lalu dijalankan lagi tanpa pemasangan lama. Catatan folder aplikasi di `sapod` baru disimpan setelah file Quadlet dan systemd berhasil diperbarui, jadi sinkronisasi ulang mencoba lagi bila gagal. Akun aplikasi tidak pernah diterima di daftar izin biasa (`read`/`write`), hanya lewat `apps`.
- **Sisa akses** (misalnya penghapusan aplikasi yang gagal di tengah jalan) dicabut saat aplikasi dipasang lagi, selalu bisa dicabut dengan daftar kosong, dan tidak menahan penghapusan folder bila aplikasinya tidak terpasang.
- **Batas yang diketahui**: aplikasi yang tidak punya login (miniserve) atau belum disiapkan (Jellyfin sebelum wizard-nya) membuka folder yang diberikan ke siapa pun yang bisa mencapai port-nya, termasuk container aplikasi lain lewat jaringan Podman. Panel memperingatkannya di dialog folder. Pemisahan jaringan antar-aplikasi dan firewall menyusul. Pemasangan baca saja tidak rekursif untuk mount lain di dalam folder (shared folder Sapo tidak berisi dataset anak; ACL tetap berlaku).
- Belum: akses perangkat (misalnya `/dev/dri` untuk transcoding perangkat keras Jellyfin), reverse proxy HTTPS, dan firewall untuk port aplikasi.
