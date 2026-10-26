# CLAUDE.md — panduan untuk Claude Code di repo Sapo

Sapo by Mehuli adalah sistem operasi NAS open source (GPL-3.0) ala Synology DSM di atas Debian minimal. Pemilik proyek: PT. Mehuli Data Media.

## Bahasa
- Balas pengguna dalam **Bahasa Indonesia**.
- Komentar kode, pesan commit, teks dokumentasi: Bahasa Indonesia. Teks UI selalu lewat `web/src/i18n/id.json` dan `en.json`.

## Prinsip yang tidak boleh dilanggar
1. **Data dulu.** Tidak ada perubahan yang boleh mempertaruhkan data pengguna. Operasi destruktif wajib konfirmasi + log audit.
2. **Semua lewat web interface, dengan wizard dan petunjuk.** Fitur yang hanya bisa diatur lewat terminal = bug. Wizard didefinisikan sebagai data di `wizards/*.yaml`.
3. **Konfigurasi sebagai data, minim hard-code.** Nama, daftar, angka kebijakan, jadwal, label, paket per fitur → YAML/JSON/database, bukan di kode. Contoh: `config/sapo.yaml`, `features/*.yaml`, `branding.yaml`, `web/src/apps/apps.json`.
4. **Pemisahan hak (ADR 0002).** Hanya `sapod` (root) yang mengubah sistem, hanya lewat operasi bernama bertipe di `internal/agent`. Jangan pernah merangkai input ke string shell; jalankan program eksternal dengan `exec.Command(prog, "--", args...)`. `sapo-api` tidak pernah berjalan sebagai root.
5. **Keamanan berlapis.** Lihat `docs/adr/0003-auth-and-first-run.md` dan `docs/security/`. Perubahan pada `sapod`, autentikasi, atau berbagi file wajib disertai test dan review keamanan.

## Arsitektur singkat
- `cmd/sapod`, `internal/agent` — agen root, unix socket `/run/sapo/sapod.sock` (0660, grup `sapo`), JSON per baris.
- `cmd/sapo-console`, `internal/console` — menu darurat root di terminal fisik (ADR 0008); tidak menerima masukan dari jaringan.
- `cmd/sapo-api`, `internal/api`, `internal/auth`, `internal/audit`, `internal/config` — server panel & API (login Argon2id + TOTP, CSRF header `X-Sapo-Request`, validasi Host, allowlist jaringan).
- `web/` — Vue 3 + TypeScript + shadcn-vue (Tailwind v4, Reka UI). Mode Desktop berjendela ala DSM (default) + mode Ringkas; HP selalu layar penuh.
- `packaging/deb/` — paket `.deb`; konfigurasi berlapis `/usr/share/sapo/sapo.yaml` (bawaan) → `/etc/sapo/sapo.yaml` (admin).
- Keputusan arsitektur di `docs/adr/`. Rencana besar ada di dokumen rancangan (Claude Docs) dan Project "Nas".

## Perintah
```bash
make build      # sapod, sapo-api, panel
make dev        # mode pengembangan (tanpa HTTPS); panel di http://localhost:5173
make check      # gofmt + vet + typecheck + test (wajib sebelum commit)
make deb        # paket .deb ke dist/ (ARCH=amd64|arm64)
make iso        # ISO live dev ke dist/ (Debian + live-build, memakai sudo)
go test -race ./...
```
- Dependensi Go di-vendor (`vendor/`). Menambah dependensi: `go mod tidy && go mod vendor`, sertakan `go.sum` dan `vendor/` di commit yang sama.
- Fitur yang menyentuh disk hanya diuji di VM dev (`deploy/dev/`), **tidak pernah** di mesin berisi data.

## Commit
- `git commit -s` (DCO). Pesan: kalimat perintah singkat, Bahasa Indonesia.

## Status & langkah berikutnya (per 24 Okt 2026)
Sudah: kerangka, login + wizard setup awal + 2FA, paket `.deb`, tiga ronde review keamanan (`docs/security/2026-10-23-review.md`), repo publik `mehuli-id/sapo`, ISO live dev (`packaging/iso/`, workflow **ISO dev**: build + uji boot QEMU, artifact `sapo-iso-dev`), Storage Manager baca saja (`storage.disks`/`storage.smart` di `sapod`, program eksternal hanya lewat `runTool` di `internal/agent/tool.go`), HTTPS wajib di LAN (ADR 0004: sertifikat self-signed, HTTP hanya halaman petunjuk, sidik jari di layar konsol lewat `sapod`), pemulihan 2FA (kode cadangan + aplikasi Keamanan Akun), repo APT bertanda tangan di GitHub Pages (`packaging/apt/`, workflow **Rilis**: tag `v*` → stable, manual → dev). Pool ZFS dari panel (ADR 0005): buat pool (wizard), status, scrub, ganti disk, hapus pool; operasi destruktif di `internal/agent/pool.go` wajib lolos pemeriksaan identitas disk, status pakai, `O_EXCL`, konfirmasi hapus, password, dan log audit; uji asap ISO menjalankan semuanya di ZFS sungguhan. Pengguna dan grup (peran admin/pengguna, langkah wajib akun). Shared folder + SMB (ADR 0006): akun sistem Linux dikelola `sapod` (`internal/agent/accounts.go`, hanya akun yang tercatat di state file), folder = dataset ZFS bertanda `sapo:share` dengan ACL POSIX, `smb.conf` ditulis utuh dari templat; uji asap ISO membuka folder lewat `smbclient`. NFS (ADR 0007): v4 saja, per folder daftar alamat klien dan satu pengguna Sapo; semua akses dari klien dipetakan ke akun itu (`all_squash`), izinnya izin efektif pengguna itu; file ekspor ditulis utuh oleh `sapod`; uji asap ISO memasang folder lewat NFS dari host.
Menu darurat di konsol NAS (ADR 0008): `sapo-console` (root, hanya keyboard/layar fisik, `sapo-console@tty1`) untuk reset password admin, reset jaringan, restart, matikan; uji asap ISO memakainya lewat port serial.
Pengaturan jaringan dari panel (ADR 0009): nama host, DHCP/alamat tetap, DNS; `/etc/network/interfaces` ditulis utuh oleh `sapod`; setiap perubahan wajib dikonfirmasi (token) atau dikembalikan otomatis; uji asap ISO menguji keduanya.
Resource Monitor: `system.stats` di `sapod` (sampel /proc hanya selama ada yang membaca), grafik CPU, memori, jaringan, disk di panel.
File Station (ADR 0010): operasi file dijalankan `sapod fs-helper` sebagai akun Linux pengguna (izin ditegakkan kernel), path lewat `openat2` di bawah akar folder, isi file lewat file descriptor (SCM_RIGHTS) ke `sapo-api`.
Control Panel: pintasan aplikasi pengaturan, SMB/NFS menyala/mati (settings.json di sapo-api, `SMBOff`/`NFSOff` di shares.apply), zona waktu dan NTP (`time.*` lewat timedatectl).
Pembaruan sistem (ADR 0011): periksa (`apt-get update` + simulasi) dan pasang (`apt-get upgrade --with-new-pkgs`, tanpa menghapus paket) lewat `systemd-run` di unit `sapo-updates` tersendiri (`sapod apt-run`), agar tidak terputus saat paket Sapo sendiri me-restart `sapod`.
App Center (ADR 0012): katalog kurasi `apps/*.yaml` (image dikunci digest), Podman + Quadlet, setiap aplikasi akun sendiri (`sapo-app-<id>`, UID 30000–30999) tanpa capability, data di dataset `<pool>/sapo-apps/<id>`; tahap 2: admin memberi aplikasi shared folder (baca/tulis) lewat entri ACL akun aplikasi dan `Volume=` hanya untuk folder itu, disinkronkan bersama `shares.apply`.
Sapo Drive tahap 1 (ADR 0013): folder pribadi per pengguna, dataset `<pool>/sapo-home/<nama>` (0700, kuota ZFS) lewat `homes.apply` saat sinkronisasi, dibuka lewat SMB `[homes]` dan File Station ("Drive saya"); folder pengguna yang dihapus disimpan sampai admin menghapusnya. Tahap 2: WebDAV di `https://<nas>/dav/` (sapo-api, operasi tetap lewat `sapod fs-helper` sebagai pengguna), login dengan password aplikasi per perangkat (Keamanan Akun), sertifikat bisa diunduh untuk dipercaya perangkat lain.
Snapshot terjadwal (ADR 0014): jadwal per shared folder dan untuk semua folder pribadi di properti ZFS `sapo:snap`, `sapod` mengambil/membuang snapshot setiap menit, snapshot manual, hapus dengan password, "Previous Versions" lewat Samba `shadow_copy2`.
Pemulihan di File Station: "Versi sebelumnya" (snapshot sebagai akar baca saja `FilesTarget.Snapshot`) dan `files.restore` (salin sebagai pengguna, nama baru bila sudah ada).
Penyimpanan eksternal (ADR 0015): drive USB exFAT/FAT/NTFS dipasang `sapod` sebagai folder eksternal (grup + ro/rw lewat opsi mount, `nosuid,nodev,noexec`), diingat per UUID, dibuka lewat File Station/WebDAV/SMB, dilepas dengan aman; disk USB tidak untuk pool kecuali diizinkan, identitas disk kembar ditolak.
Installer ke disk (ADR 0016): wizard di Storage Manager saat berjalan dari ISO; `sapod install-run` (unit `sapo-install`) membuat partisi BIOS/EFI/root ext4, menyalin squashfs ISO dan pengaturan sesi live (`agent.install.copy_paths`), memasang GRUB BIOS+UEFI; uji asap ISO boot ulang dari disk itu tanpa ISO.
Backup (ADR 0017): aplikasi Backup, tugas lewat wizard `backup-create`; `sapod` menyalin shared folder ke `<pool lain>/sapo-backup/<id>` harian dengan `zfs send`/`receive` (penuh lalu inkremental, tanpa `-F`), salinan tidak dipasang dan tidak dibagikan; pemulihan selalu ke shared folder baru; folder dan pool yang dipakai tugas tidak bisa dihapus; uji asap ISO menjalankan backup, pemulihan, dan baca lewat SMB.
Notifikasi email (ADR 0018): sapo-api memeriksa pool, SMART, dan backup lewat operasi baca `sapod`, mengirim lewat SMTP (`internal/mail`, STARTTLS wajib atau TLS; login tidak pernah tanpa enkripsi), satu email per masalah baru plus email pulih; wizard `notify-setup`, bagian Notifikasi di Control Panel; uji asap ISO memakai server SMTP kecil di host.
Pemeriksaan pembaruan harian (ADR 0011, tambahan): `sapo-api` meminta `updates.check` pada jam yang diatur di Control Panel, hasil terakhir tampil di panel dan memicu email `updates_available`; tidak pernah memasang otomatis.
Backup ke NAS lain (ADR 0019): `zfs send`/`receive` di `sapod`, data dibawa sapo-api lewat HTTPS dengan file descriptor (tanpa SSH); kode pemasangan berisi sidik jari sertifikat penerima; partner hanya bisa mengirim dan membaca daftar salinannya (tidak bisa menghapus; retensi menurut waktu terima); header stream diperiksa `sapod` (tolak `-R`/`-p`/clone/raw); pemulihan di penerima ke shared folder baru; uji asap ISO memakai satu VM sebagai pengirim dan penerima.
Backup ke S3/cloud (ADR 0020): restic (fitur `cloud-backup`) dijalankan `sapod` lewat `systemd-run` di unit terkunci (backup hanya `CAP_DAC_READ_SEARCH`, pemulihan hanya menulis folder baru), rahasia lewat `LoadCredential`, password enkripsi acak tampil sekali, "sambungkan repo yang ada" untuk NAS pengganti; uji asap ISO memakai server S3 SeaweedFS di host. Pemulihan satu file/folder: isi salinan ditelusuri lewat `cloud.ls`, path yang dipilih dipulihkan (`restic restore --include`, pola di-escape) ke shared folder baru dengan letak yang sama.
Berikutnya: belum ditentukan (usulan: pulihkan balik ke NAS asal dari NAS lain).
