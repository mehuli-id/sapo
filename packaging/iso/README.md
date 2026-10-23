# ISO

## ISO live dev (sekarang)

ISO live Debian 13 berisi paket `sapo`, untuk mencoba panel dan wizard setup awal tanpa memasang apa pun. Sistem berjalan dari RAM: **tidak ada yang ditulis ke disk**, dan semua perubahan (akun, pengaturan) hilang saat dimatikan.

### Mengunduh

GitHub Actions membangun ISO di setiap push ke `main` (workflow **ISO dev**). Unduh artifact `sapo-iso-dev` dari run terbaru di tab *Actions*. Artifact disimpan 14 hari. Sebelum diunggah, setiap ISO di-boot di QEMU dan baru lolos bila panel menjawab (`smoke-test.sh`).

Build manual: tab *Actions* → **ISO dev** → *Run workflow*.

### Mencoba di VM

Selalu di VM, bukan di mesin berisi data. Dengan QEMU:

```bash
qemu-system-x86_64 -accel kvm -m 2048 -smp 2 -cdrom sapo-dev_<versi>_amd64.iso \
  -nic user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:5000-:5000
```

Lalu buka `http://localhost:5000` dan ikuti wizard setup awal. Di VirtualBox, UTM, atau Proxmox, pasang ISO sebagai CD dengan jaringan bridge; alamat panel tampil di layar konsol VM (`http://<IP>:5000` atau `http://sapo.local:5000`).

Untuk menguji fitur storage, tambahkan disk virtual kosong ke VM (lihat `deploy/dev/`).

Login konsol untuk debugging: `user` / `live` (bawaan live-config, dengan sudo). SSH tidak dipasang, karena password ini bawaan dan diketahui umum.

### Membangun sendiri

Di Debian 13 dengan paket `live-build`:

```bash
make iso        # paket .deb, lalu ISO (langkah ISO memakai sudo)
```

Hasilnya `dist/sapo-dev_<versi>_amd64.iso` beserta `.sha256`. Di macOS atau distro lain, pakai container Debian seperti di `.github/workflows/iso.yml` (`docker run --privileged debian:trixie …`).

### Isi dan pengaturan

| File | Isi |
|---|---|
| `iso.conf` | Nama ISO, arsitektur, versi Debian, mirror, parameter boot (nama host `sapo`, tanpa autologin, log ke serial) |
| `packages.list` | Paket sistem live; paket sistem inti ikut lewat `Depends` paket `sapo` |
| `includes.chroot/` | File yang disalin ke sistem live, misalnya alamat panel di layar login (`/etc/issue.d/sapo.issue`) |
| `build.sh` | Menjalankan `lb config` dan `lb build` dari data di atas |
| `smoke-test.sh` | Boot ISO di QEMU tanpa disk, lalu cek `/api/v1/health`, `/api/v1/auth/status`, dan halaman panel |

## Installer ISO (Fase 3)

Belum diimplementasikan. Rencananya dibangun di atas ISO ini, berisi:

- installer dengan wizard di layar (pilih disk sistem, mirror opsional, jaringan),
- repo APT lokal berisi paket fitur utama agar fitur bisa diaktifkan tanpa internet,
- sistem di atas ZFS boot environments untuk rollback update.
