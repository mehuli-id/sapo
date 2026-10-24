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
Berikutnya (urutan disepakati): App Center, Sapo Drive; lalu:
1. Snapshot ZFS terjadwal untuk shared folder (dengan pemulihan file).
2. Installer ke disk (ISO sekarang hanya berjalan dari RAM).
