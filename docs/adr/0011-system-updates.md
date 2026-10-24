# ADR 0011: Pembaruan sistem dari panel

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

NAS harus mendapat perbaikan keamanan Debian dan versi Sapo baru tanpa terminal (prinsip 2). Memasang pembaruan mengubah seluruh sistem, bisa lama, dan bisa memperbarui paket Sapo sendiri, yang me-restart `sapod` dan `sapo-api` di tengah pemasangan.

## Keputusan

- **Periksa** (`updates.check`): `apt-get update`, lalu simulasi `apt-get -s upgrade --with-new-pkgs` untuk daftar paket (nama, versi lama, versi baru). Admin, tanpa password (tidak mengubah paket).
- **Pasang** (`updates.install`): butuh password admin dan tercatat di log audit. `sapod` menjalankan dirinya sendiri (`sapod apt-run`) lewat `systemd-run` di **unit sementara tersendiri** (`agent.updates.unit`, bawaan `sapo-updates.service`). Unit itu bukan bagian cgroup `sapod`, jadi tidak ikut dihentikan saat paket Sapo diperbarui dan `sapod` di-restart. Hanya satu pemasangan atau pemeriksaan berjalan pada satu waktu.
  - `apt-get upgrade --with-new-pkgs`: paket baru yang dibutuhkan boleh dipasang, **tidak pernah menghapus paket** (bukan `dist-upgrade`). Pemutakhiran rilis Debian di luar cakupan ini.
  - Non-interaktif (`DEBIAN_FRONTEND=noninteractive`), file konfigurasi yang diubah admin dipertahankan (`--force-confdef --force-confold`).
  - Semua argumen apt tetap di kode; dari panel hanya perintah "periksa" dan "pasang".
- **Status** (`updates.status`): unit masih aktif atau tidak, hasil terakhir (`result_file`, ditulis `apt-run` di awal dan akhir), dan 40 baris terakhir log (`log_file`, karakter kendali dibuang). Panel terus menanyakan status selama pemasangan, termasuk saat `sapo-api` sedang dinyalakan ulang.

## Alasan

- Unit terpisah adalah satu-satunya cara agar `dpkg` tidak terputus di tengah jalan ketika paket yang sedang dipasang me-restart layanan pemanggilnya.
- `upgrade` (bukan `dist-upgrade`) menghindari penghapusan paket diam-diam, misalnya paket yang dibutuhkan fitur Sapo.

## Konsekuensi

- Pembaruan kernel atau ZFS butuh restart NAS agar berlaku; panel belum memberi tahu (menyusul).
- Pembaruan otomatis terjadwal belum ada.
- Uji asap ISO selalu memeriksa pembaruan, tetapi hanya memasangnya bila daftarnya kecil dan tidak menyentuh kernel, ZFS, atau Sapo.
