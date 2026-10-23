# VM pengembangan

Semua fitur yang menyentuh disk (pool, RAID, snapshot, ganti disk) diuji di VM ini, dengan disk data virtual. **Jangan pernah menguji di mesin yang berisi data penting.**

## Menyiapkan

Kebutuhan (Linux): `qemu-system-x86`, `qemu-utils`, `cloud-image-utils`, `curl`, KVM aktif, dan kunci SSH di `~/.ssh/id_ed25519.pub`.

```bash
deploy/dev/create-vm.sh          # unduh Debian 13 cloud image, buat disk sistem + 6 disk data 8 GB
deploy/dev/create-vm.sh run      # jalankan VM (keluar: Ctrl+A lalu X)
ssh -p 2222 sapo@localhost       # masuk ke VM
```

Panel di dalam VM diteruskan ke `http://localhost:5000` di mesin Anda.

Jumlah dan ukuran disk diatur lewat variabel lingkungan, misalnya `DATA_DISKS=4 DATA_DISK_SIZE=16G deploy/dev/create-vm.sh`. Semua variabel ada di bagian atas skrip.

## Mulai ulang uji storage

```bash
deploy/dev/create-vm.sh reset    # hapus dan buat ulang semua disk data
```

## Catatan

- Disk data diberi serial `SAPO-DATA-1` sampai `SAPO-DATA-6`, sehingga mudah dikenali di Storage Manager.
- Untuk simulasi disk rusak, matikan VM lalu hapus salah satu file `dataN.qcow2` dan jalankan ulang.
- Skrip ini belum diuji di macOS atau Windows; kontribusi untuk dukungan lintas platform dipersilakan.
