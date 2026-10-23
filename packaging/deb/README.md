# Paket .deb

Paket `sapo` untuk Debian stable (Fase 1: untuk penguji dan kontributor).

## Isi paket

| Lokasi di sistem | Sumber di repo |
|---|---|
| `/usr/lib/sapo/sapod`, `/usr/lib/sapo/sapo-api` | `bin/` (hasil `make go`) |
| `/usr/share/sapo/web/` | `web/dist/` (hasil `make web`) |
| `/usr/share/sapo/features/` | `features/` |
| `/usr/share/sapo/wizards/` | `wizards/` |
| `/usr/share/sapo/branding.yaml` | `branding.yaml` |
| `/etc/sapo/sapo.yaml` (conffile) | `config/sapo.yaml` |
| `/lib/systemd/system/sapod.service`, `sapo-api.service` | `packaging/deb/systemd/` |

## Saat instalasi (postinst)

1. Membuat user dan grup sistem `sapo` (tanpa shell login).
2. Memasang paket fitur `core` dan mesin storage yang dipilih.
3. Mengaktifkan `sapod.service` dan `sapo-api.service`.

Unit systemd sudah dikeraskan: `sapo-api` berjalan sebagai user `sapo` tanpa capability apa pun, dan `sapod` tidak punya akses jaringan selain unix socket.

Skrip build paket (`dpkg-deb`/`nfpm`) menyusul di Fase 1.
