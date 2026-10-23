# Berkontribusi ke Sapo

Terima kasih sudah tertarik membantu! Diskusi dan dokumentasi boleh dalam bahasa Indonesia atau Inggris.

## Prinsip yang wajib dijaga

1. **Data dulu.** Tidak ada perubahan yang boleh mempertaruhkan data pengguna. Operasi destruktif wajib dikonfirmasi dan tercatat di log audit.
2. **Semua lewat web interface.** Fitur yang hanya bisa diatur lewat terminal dianggap bug. Setiap setup punya wizard dan petunjuk.
3. **Konfigurasi sebagai data.** Nama, daftar, jadwal, label, teks UI, langkah wizard, dan daftar paket per fitur disimpan sebagai data (YAML/JSON/database), bukan ditulis di kode.
4. **Pemisahan hak.** Hanya `sapod` yang boleh mengubah sistem, dan hanya lewat operasi bernama bertipe. Jangan pernah merangkai input menjadi string shell.

## Menyiapkan lingkungan

```bash
git clone https://github.com/mehuli-id/sapo.git
cd sapo
make build
make dev
```

Fitur yang menyentuh disk diuji di VM dev — lihat `deploy/dev/README.md`.

## Alur kontribusi

1. Buka issue atau diskusi dulu untuk perubahan besar. Keputusan arsitektur dicatat sebagai ADR di `docs/adr/`.
2. Buat branch dari `main`, kerjakan, tambahkan test.
3. Jalankan `make check` (lint + test) sebelum membuka pull request.
4. Setiap commit wajib memakai DCO: tambahkan `Signed-off-by` dengan `git commit -s`.
5. Perubahan pada `sapod`, autentikasi, dan berbagi file wajib direview dua maintainer.

## Dependensi

Dependensi Go disimpan di `vendor/` agar build bisa diulang dan berjalan tanpa internet (penting untuk paket Debian dan ISO). Tambah dependensi seminimal mungkin; setelah menambah atau memperbarui, jalankan `go mod tidy && go mod vendor` dan sertakan perubahan `go.sum` serta `vendor/` di commit yang sama.

## Gaya kode

- Go: `gofmt`, `go vet`, error dibungkus dengan konteks, tanpa `panic` di jalur normal.
- Frontend: TypeScript strict, komponen shadcn-vue di `web/src/components/ui`, teks UI hanya dari `web/src/i18n/*.json`.
- Pesan commit: kalimat perintah singkat, misalnya `agent: tambah operasi disk.list`.

## Lisensi kontribusi

Dengan berkontribusi, Anda setuju kontribusi Anda dilisensikan di bawah GPL-3.0 dan menyatakan hak Anda atas kode tersebut melalui [Developer Certificate of Origin](https://developercertificate.org/).
