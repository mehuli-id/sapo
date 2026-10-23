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

## Status & langkah berikutnya (per 23 Okt 2026)
Sudah: kerangka, login + wizard setup awal + 2FA, paket `.deb`, tiga ronde review keamanan (`docs/security/2026-10-23-review.md`), repo publik `mehuli-id/sapo`, ISO live dev (`packaging/iso/`, workflow **ISO dev**: build + uji boot QEMU, artifact `sapo-iso-dev`), Storage Manager baca saja (`storage.disks`/`storage.smart` di `sapod`, program eksternal hanya lewat `runTool` di `internal/agent/tool.go`), HTTPS wajib di LAN (ADR 0004: sertifikat self-signed, HTTP hanya halaman petunjuk, sidik jari di layar konsol lewat `sapod`), pemulihan 2FA (kode cadangan + aplikasi Keamanan Akun), repo APT bertanda tangan di GitHub Pages (`packaging/apt/`, workflow **Rilis**: tag `v*` → stable, manual → dev).
Berikutnya:
1. Menu darurat di konsol NAS (reset jaringan, reset password admin, restart) untuk pemulihan bila password dan kode cadangan hilang.
