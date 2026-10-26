# ISO

Ada dua jenis ISO dari skrip yang sama (`FLAVOR` di `build.sh`, data di `iso.conf`):

- **Rilis** (`sapo_<versi>_amd64.iso`): dari tag `v*`, dilampirkan ke halaman [GitHub Releases](https://github.com/mehuli-id/sapo/releases). Untuk memasang Sapo ke komputer. Tanpa akun debug.
- **Dev** (`sapo-dev_<versi>_amd64.iso`): dari setiap push ke `main`, untuk pengembang dan penguji. Ada akun debug `user` di konsol.

Keduanya berisi firmware kartu jaringan dan microcode CPU (area `non-free-firmware` Debian), dan keduanya lolos uji asap QEMU yang sama sebelum diterbitkan.

## Memasang Sapo ke komputer (ISO rilis)

Komputer Intel/AMD 64-bit (BIOS atau UEFI), RAM minimal 4 GB, disk sistem minimal 16 GB, dan **kabel LAN** (Wi-Fi belum didukung). Sebaiknya satu disk lagi (atau lebih) untuk data, karena disk sistem tidak dipakai untuk pool.

1. **Unduh** `sapo_<versi>_amd64.iso` dan `.sha256` dari GitHub Releases, lalu periksa: `shasum -a 256 -c sapo_<versi>_amd64.iso.sha256` (macOS) atau `sha256sum -c …` (Linux).
2. **Tulis ke flashdisk** (minimal 2 GB; seluruh isinya terhapus). ISO ini *hybrid*: ditulis apa adanya, tanpa diekstrak.
   - Paling mudah: balenaEtcher, Rufus (mode DD), atau GNOME Disks.
   - macOS: `diskutil list external` untuk mencari flashdisk, lalu `diskutil unmountDisk /dev/diskN` dan `sudo dd if=sapo_<versi>_amd64.iso of=/dev/rdiskN bs=4m`.
   - Linux: `sudo dd if=sapo_<versi>_amd64.iso of=/dev/sdX bs=4M conv=fsync`.
   - Periksa nomor disk dua kali: `dd` ke disk yang salah menghapusnya.
3. **Boot dari flashdisk** lewat menu boot komputer (biasanya F12, F11, F8, atau Esc). Bila ditolak, matikan **Secure Boot** di BIOS/UEFI (belum didukung).
4. Setelah boot, layar konsol menampilkan alamat panel (`https://<IP>:5001`) dan sidik jari sertifikat. Buka dari komputer lain di jaringan yang sama, cocokkan sidik jarinya, lalu jalankan setup awal (akun admin dan 2FA).
5. **Storage Manager → Pasang ke disk** (ADR 0016): pilih disk sistem, ketik ulang namanya, masukkan password. Disk itu dihapus total. Pengaturan dari sesi USB (admin, pengguna, pool) ikut tersalin.
6. Cabut flashdisk dan restart. Sapo boot dari disk. Buat pool data di Storage Manager bila belum.

Pembaruan Sapo berikutnya datang dari repo APT saluran *stable* (Control Panel → Pembaruan).

## ISO live dev

ISO live Debian 13 berisi paket `sapo` dan ZFS, untuk mencoba panel tanpa memasang apa pun. Sistem berjalan dari RAM, dan akun serta pengaturan hilang saat dimatikan.

> **Peringatan:** wizard **Buat pool penyimpanan** di Storage Manager benar-benar menghapus disk yang dipilih. Jalankan ISO ini hanya di VM dengan disk virtual kosong, jangan di komputer yang disknya berisi data.

### Mengunduh

GitHub Actions membangun ISO di setiap push ke `main` (workflow **ISO dev**). Unduh artifact `sapo-iso-dev` dari run terbaru di tab *Actions*. Artifact disimpan 14 hari. Sebelum diunggah, setiap ISO di-boot di QEMU dan baru lolos bila panel menjawab (`smoke-test.sh`).

Build manual: tab *Actions* → **ISO dev** → *Run workflow*.

### Mencoba di VM

Selalu di VM, bukan di mesin berisi data. Dengan QEMU:

```bash
qemu-system-x86_64 -accel kvm -m 2048 -smp 2 -cdrom sapo-dev_<versi>_amd64.iso \
  -nic user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:5000-:5000,hostfwd=tcp:127.0.0.1:5001-:5001
```

Untuk mencoba pool, tambahkan disk virtual kosong, misalnya dua file 4 GB:

```bash
truncate -s 4G disk1.img disk2.img
# tambahkan ke perintah qemu di atas:
#   -drive file=disk1.img,if=virtio,format=raw -drive file=disk2.img,if=virtio,format=raw
```

Lalu buka `http://localhost:5000`. Halaman itu menjelaskan peringatan sertifikat dan menampilkan sidik jarinya, lalu mengarahkan ke `https://localhost:5001` (HTTPS wajib, lihat ADR 0004). Setelah itu ikuti wizard setup awal.

Di VirtualBox, UTM, atau Proxmox, pasang ISO sebagai CD dengan jaringan bridge. Alamat panel dan sidik jari sertifikat tampil di layar konsol VM (`https://<IP>:5001` atau `https://sapo.local:5001`).

Untuk menguji fitur storage, tambahkan disk virtual kosong ke VM (lihat `deploy/dev/`).

#### Lewat Docker (misalnya di macOS)

ISO di-boot dengan QEMU di dalam container Debian. Di Mac Apple Silicon, x86 diemulasi (tanpa KVM), tetapi boot tetap sekitar 40 detik.

```bash
docker run -d --rm --name sapo-iso -p 127.0.0.1:5080:5080 -p 127.0.0.1:5081:5081 -v "$PWD:/iso:ro" debian:trixie sh -c '
  apt-get update -qq && apt-get install -y -qq --no-install-recommends qemu-system-x86 socat >/dev/null
  socat TCP-LISTEN:5080,fork,reuseaddr TCP:127.0.0.1:5000 &
  socat TCP-LISTEN:5081,fork,reuseaddr TCP:127.0.0.1:5001 &
  exec qemu-system-x86_64 -accel tcg -m 2048 -smp 4 -cdrom /iso/sapo-dev_<versi>_amd64.iso -boot d \
    -nic user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:5000-:5000,hostfwd=tcp:127.0.0.1:5001-:5001 \
    -display none -serial stdio'
docker logs -f sapo-iso      # layar konsol VM
docker rm -f sapo-iso        # berhenti (semua isi hilang)
```

Jalankan dari folder berisi ISO, lalu buka `http://localhost:5080`. Halaman itu mengarahkan ke panel HTTPS di `https://localhost:5081`. Port 5080/5081 dipakai karena di macOS port 5000 biasanya sudah dipakai AirPlay Receiver.

Relay `socat` diperlukan. Tanpa relay, koneksi lewat port Docker sampai ke VM dari jaringan Docker (`172.17.0.0/16`). Sapo sengaja menolak jaringan itu (`api.denied_networks`), sehingga panel menjawab **403 `network_not_allowed`**. Dengan relay, koneksi datang dari loopback container, dan VM melihatnya sebagai `10.0.2.2` (diizinkan). Jangan longgarkan `denied_networks` untuk mengakalinya.

Login konsol untuk debugging (hanya ISO dev): `user` / `live` (bawaan live-config, dengan sudo). SSH tidak dipasang, karena password ini bawaan dan diketahui umum. Installer menghapus akun ini, dan ISO rilis tidak memilikinya.

### Membangun sendiri

Di Debian 13 dengan paket `live-build`:

```bash
make iso        # paket .deb, lalu ISO dev (langkah ISO memakai sudo)
# ISO rilis: make deb ARCH=amd64 && sudo FLAVOR=release packaging/iso/build.sh
```

Hasilnya `dist/sapo-dev_<versi>_amd64.iso` (atau `sapo_<versi>_amd64.iso`) beserta `.sha256`. Untuk menguji jenis rilis sebelum membuat tag: tab *Actions* → **ISO dev** → *Run workflow* → jenis `release`. Di macOS atau distro lain, pakai container Debian seperti di `.github/workflows/iso.yml` (`docker run --privileged debian:trixie …`).

### Isi dan pengaturan

| File | Isi |
|---|---|
| `iso.conf` | Nama, label, dan teks layar login per jenis (dev/rilis), arsitektur, versi Debian, area arsip, mirror, parameter boot (nama host `sapo`, tanpa autologin, log ke serial) |
| `packages.list` | Paket sistem live (termasuk firmware); paket sistem inti ikut lewat `Depends` paket `sapo` |
| `packages-dev.list` | Paket tambahan hanya untuk ISO dev (akun debug) |
| `includes.chroot/` | File yang disalin ke sistem live. Teks jenis ISO di layar login (`/etc/issue.d/sapo-iso.issue`) ditulis `build.sh`; alamat panel dan sidik jari ditulis `sapod` (`/etc/issue.d/sapo.issue` dari paket) |
| `build.sh` | Menjalankan `lb config` dan `lb build` dari data di atas |
| `smoke-test.sh` | Boot ISO di QEMU dengan tiga disk virtual sementara. Cek panel dan API lewat HTTPS, HTTP hanya halaman petunjuk, dan sidik jari (diumumkan, dipakai server, dan di konsol). Lalu jalankan setup awal lewat API, buat pool ZFS mirror, pastikan disk berisi data dan password salah ditolak, dan jalankan scrub (ADR 0005) |
| `hooks/` | Pemeriksaan di dalam chroot saat build, misalnya modul ZFS terkompilasi untuk kernel ISO |

## Rencana berikutnya

Installer ke disk sudah ada di panel (ADR 0016). Yang belum:

- Secure Boot (shim bertanda tangan) dan Wi-Fi,
- repo APT lokal berisi paket fitur utama agar fitur bisa diaktifkan tanpa internet,
- sistem di atas ZFS boot environments untuk rollback update.
