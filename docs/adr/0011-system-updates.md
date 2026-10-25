# ADR 0011: Pembaruan sistem dari panel

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

NAS harus mendapat perbaikan keamanan Debian dan versi Sapo baru tanpa terminal (prinsip 2). Memasang pembaruan mengubah seluruh sistem, bisa lama, dan bisa memperbarui paket Sapo sendiri, yang me-restart `sapod` dan `sapo-api` di tengah pemasangan.

## Keputusan

- **Periksa** (`updates.check`): `apt-get update`, lalu simulasi `apt-get -s upgrade --with-new-pkgs` untuk daftar paket (nama, versi lama, versi baru). Admin, tanpa password (tidak mengubah paket).
- **Pasang** (`updates.install`): butuh password admin dan tercatat di log audit. `sapod` menjalankan dirinya sendiri (`sapod apt-run`) lewat `systemd-run` di **unit sementara tersendiri** (`agent.updates.unit`, bawaan `sapo-updates.service`). Unit itu bukan bagian cgroup `sapod`, jadi tidak ikut dihentikan saat paket Sapo diperbarui dan `sapod` di-restart. Hanya satu pemasangan atau pemeriksaan berjalan pada satu waktu.
  - `apt-get upgrade --with-new-pkgs`: paket baru yang dibutuhkan boleh dipasang, **tidak pernah menghapus paket** (bukan `dist-upgrade`). Pemutakhiran rilis Debian di luar cakupan ini.
  - Pemasangan dpkg sebelumnya yang terputus dilanjutkan dulu (`dpkg --configure -a`), agar tidak pernah butuh terminal.
  - Non-interaktif (`DEBIAN_FRONTEND=noninteractive`), file konfigurasi yang diubah admin dipertahankan (`--force-confdef --force-confold`, `UCF_FORCE_CONFFOLD`), needrestart hanya mendaftar layanan.
  - Menunggu kunci apt sampai 2 menit (misalnya apt-daily berjalan); unit dihentikan systemd setelah `install_timeout_seconds` (bawaan 3 jam).
  - Semua argumen apt tetap di kode; dari panel hanya perintah "periksa" dan "pasang".
- **Status** (`updates.status`): unit masih aktif atau tidak, hasil terakhir (`result_file`, ditulis `apt-run` di awal dan akhir), dan 40 baris terakhir log (`log_file`, karakter kendali dibuang). Panel terus menanyakan status selama pemasangan, termasuk saat `sapo-api` sedang dinyalakan ulang. Unit yang berhenti tanpa hasil akhir dilaporkan "terputus".

## Alasan

- Unit terpisah adalah satu-satunya cara agar `dpkg` tidak terputus di tengah jalan ketika paket yang sedang dipasang me-restart layanan pemanggilnya.
- `upgrade` (bukan `dist-upgrade`) menghindari penghapusan paket diam-diam, misalnya paket yang dibutuhkan fitur Sapo.

## Konsekuensi

- Pembaruan kernel atau ZFS butuh restart NAS agar berlaku; panel belum memberi tahu (menyusul).
- Pembaruan otomatis terjadwal belum ada. **Tambahan (2026-10-25):** `sapo-api` meminta `updates.check` sekali sehari pada jam yang diatur admin (`api.update_check.default_hour`, waktu NAS dari `time.status`; diulang sejam kemudian bila gagal, langsung saat NAS menyala bila jamnya terlewat). Hasil terakhir (otomatis atau manual) disimpan di `<data_dir>/update-check.json`, tampil di Control Panel, dan menjadi kejadian notifikasi `updates_available` (ADR 0018; tanpa email "pulih"). Pemasangan tetap hanya lewat panel dengan password.
- Log audit mencatat permintaan pemasangan; hasilnya ada di status dan log pemasangan, belum di log audit.
- Uji asap ISO selalu memeriksa pembaruan, tetapi hanya memasangnya bila daftarnya kecil dan tidak menyentuh kernel, ZFS, atau Sapo.
