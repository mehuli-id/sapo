# ADR 0002: Pemisahan sapod dan sapo-api lewat operasi bernama

- **Status:** Diterima
- **Tanggal:** 2026-10-23

## Konteks

Panel NAS adalah sasaran utama penyerang. Bila proses web berjalan sebagai root atau bisa menjalankan perintah shell, satu celah web berarti kendali penuh atas data.

## Keputusan

- `sapod` berjalan sebagai root dan **hanya** mendengarkan unix socket lokal (`/run/sapo/sapod.sock`, mode 0660, grup `sapo`).
- `sapo-api` berjalan sebagai user `sapo` tanpa hak root, menangani HTTP, autentikasi, role, dan validasi.
- Komunikasi memakai protokol JSON sederhana per baris: `{"id", "op", "params"}` → `{"id", "ok", "result" | "error"}`. Kontraknya ada di `internal/proto`.
- `sapod` hanya mengenal **operasi bernama** yang terdaftar (misalnya `system.info`, nanti `disk.list`, `pool.create`). Parameter didekode ke struct bertipe dan divalidasi. Tidak ada operasi "jalankan perintah", dan argumen tidak pernah dirangkai menjadi string shell.

## Konsekuensi

- Menambah kemampuan sistem berarti menambah operasi baru di `sapod` dengan review dua maintainer.
- `sapo-api` bisa di-restart, diperbarui, atau di-sandbox ketat tanpa mengganggu agent.
- Panel dan aplikasi HP memakai API yang sama, sehingga tidak ada logika ganda.
