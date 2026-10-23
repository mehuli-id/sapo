# Installer ISO (Fase 3)

ISO dibangun dengan Debian `live-build` di atas Debian stable minimal, berisi:

- installer dengan wizard di layar (pilih disk sistem, mirror opsional, jaringan),
- repo APT lokal berisi paket fitur utama agar fitur bisa diaktifkan tanpa internet,
- sistem di atas ZFS boot environments untuk rollback update.

Belum diimplementasikan. Untuk saat ini gunakan paket `.deb` (lihat `packaging/deb/`).
