# Manifest fitur

Setiap fitur Sapo punya satu file YAML di folder ini. Saat admin mengaktifkan fitur dari panel, `sapod` memasang paket yang tercantum; saat fitur dimatikan, paket bisa dilepas. Menambah fitur baru cukup menambah manifest — tidak ada daftar paket yang ditulis di kode.

```yaml
id: smb                     # unik, dipakai panel dan API
name: { id: Berbagi file SMB, en: SMB file sharing }
group: file-sharing         # pengelompokan di panel
packages: [samba, wsdd]     # paket Debian (atau dari mirror repo Sapo)
services: [smbd, wsdd]      # layanan systemd yang dinyalakan/dimatikan
requires: []                # fitur lain yang harus aktif dulu
activation: optional        # always | setup (dipilih saat instalasi) | optional
removable: true             # boleh dilepas saat fitur dimatikan
```

Fitur dengan `activation: always` selalu terpasang dan tidak bisa dimatikan; `setup` dipilih saat instalasi (misalnya mesin storage).
