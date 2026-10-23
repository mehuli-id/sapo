# Sapo by Mehuli

**Sapo** adalah sistem operasi NAS open source (GPL-3.0) yang memberi pengalaman setara Synology DSM di hardware apa pun: storage dan RAID, shared folder, Sapo Drive (ala Google Drive/OneDrive), VPN, App Center, backup, dan monitoring — semuanya diatur lewat web interface dengan wizard dan petunjuk.

Nama Sapo diambil dari *sapo page*, lumbung padi tradisional Karo: tempat menyimpan hasil panen bersama dengan aman.

> **Status:** tahap awal (pra-alpha). Belum untuk data sungguhan. Uji hanya di VM dengan disk virtual.

*English:* Sapo is an open-source (GPL-3.0), Synology-like NAS operating system built on Debian. Everything is configured from a web interface with guided wizards. Early pre-alpha — do not use with real data.

## Arsitektur singkat

```
Panel web / App HP ──HTTPS──▶ sapo-api (user sapo, tanpa root)
                                   │ unix socket, operasi bernama
                                   ▼
                              sapod (root, satu-satunya yang menyentuh sistem)
                                   │
              Debian minimal · OpenZFS / mdadm · Samba/NFS · Docker
```

- `sapod` — System Agent. Hanya menerima operasi bernama bertipe lewat unix socket lokal; tidak ada endpoint "jalankan perintah".
- `sapo-api` — API untuk panel dan aplikasi: autentikasi, role, validasi, log audit.
- `web/` — panel Vue 3 + shadcn-vue dengan desktop berjendela ala DSM (mode Ringkas & HP tersedia).

Rincian ada di [docs/](docs/) dan dokumen rancangan proyek.

## Mencoba (pengembangan)

Kebutuhan: Go 1.24+, Node.js 22+, make.

```bash
make build          # build sapod, sapo-api, dan panel
make dev            # jalankan sapod + sapo-api + panel dalam mode pengembangan
```

Lalu buka `http://localhost:5173`. Saat pertama dibuka, panel menampilkan wizard setup awal untuk membuat akun administrator (dengan verifikasi dua langkah). Dalam mode pengembangan, `sapod` berjalan tanpa root dan hanya membaca info sistem; data akun disimpan di `.dev/data`.

Untuk menguji fitur storage, gunakan VM dev dengan disk virtual — lihat [deploy/dev/README.md](deploy/dev/README.md). **Jangan pernah menguji di mesin yang berisi data penting.**

## Struktur repo

| Folder | Isi |
|---|---|
| `cmd/` | entry point `sapod` dan `sapo-api` |
| `internal/` | kode Go: agent, api, auth, audit, config, proto (kontrak agent ↔ api) |
| `web/` | panel Vue 3 + TypeScript + shadcn-vue |
| `config/` | konfigurasi default (`sapo.yaml`) |
| `features/` | manifest fitur (paket Debian per fitur) sebagai data |
| `wizards/` | definisi wizard (langkah, isian, petunjuk) sebagai data |
| `packaging/` | paket `.deb` dan build ISO |
| `deploy/dev/` | VM pengembangan dengan disk virtual |
| `docs/` | arsitektur, ADR, panduan brand |

## Berkontribusi

Baca [CONTRIBUTING.md](CONTRIBUTING.md). Laporan celah keamanan: [SECURITY.md](SECURITY.md) — jangan lewat issue publik.

## Lisensi & merek

Kode dilisensikan di bawah [GPL-3.0](LICENSE). Nama dan logo "Sapo by Mehuli" adalah merek PT. Mehuli Data Media — lihat [TRADEMARKS.md](TRADEMARKS.md).
