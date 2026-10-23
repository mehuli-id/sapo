# ADR 0004: HTTPS wajib di jaringan lokal dengan sertifikat self-signed

- **Status:** Diterima
- **Tanggal:** 2026-10-24

## Konteks

Sampai ADR ini, panel dan password admin dikirim lewat HTTP tanpa enkripsi di LAN (ADR 0003). Dokumen rancangan menetapkan "HTTPS wajib, HSTS, TLS 1.2 ke atas" untuk mencegah penyadapan, misalnya dari Wi-Fi yang sama.

Sertifikat tepercaya (Let's Encrypt) butuh domain dan baru tersedia lewat wizard "Akses dari internet" di Fase 2. Di jaringan lokal, NAS hanya bisa memakai sertifikat yang dibuatnya sendiri, sehingga browser menampilkan peringatan pada kunjungan pertama.

## Keputusan

- **`sapo-api` membuat sertifikat self-signed sendiri** (ECDSA P-256, tanpa root) di `<data_dir>/tls/`, dengan kunci 0600. Isinya mencakup nama host, `<nama host>.local`, `localhost`, `api.hostnames`, dan alamat IP mesin saat dibuat. Sertifikat dibuat ulang hanya bila hilang atau rusak, mendekati kedaluwarsa (`api.tls.renew_before_days`), atau nama host berubah. Perubahan alamat IP tidak memicu pembuatan ulang, agar sidik jari yang sudah dicocokkan pengguna tetap sama. Pemeriksaan berjalan saat start dan setiap hari, lalu sertifikat baru dipakai tanpa restart.
- **HTTPS wajib** (`api.tls.enabled`, port `api.tls.listen`, bawaan 5001). Alamat HTTP (`api.listen`, bawaan 5000) hanya menyajikan:
  - file panel, yang menampilkan halaman petunjuk HTTPS;
  - `/api/v1/ui/config`, berisi port HTTPS dan sidik jari sertifikat;
  - `/api/v1/health`.

  Semua API lain lewat HTTP dijawab `403 https_required`, termasuk login dan setup. Jadi password tidak pernah dikirim lewat HTTP, walaupun ada yang memanggil API secara langsung.
- **Halaman petunjuk** menjelaskan peringatan sertifikat dan menampilkan sidik jari SHA-256 untuk dicocokkan. Teksnya lewat i18n. Sidik jari yang sama juga tampil di layar login konsol NAS: `sapo-api` menulis `/run/sapo-api/console.issue`, dan paket memasang tautan `/etc/issue.d/sapo.issue` ke file itu.
- **TLS 1.2 ke atas.** Cookie sesi bertanda `Secure` lewat HTTPS.
- **HSTS tidak dikirim** selama sertifikat self-signed.

## Alasan

- Mengalihkan HTTP langsung ke HTTPS membuat pengguna kaget oleh peringatan browser tanpa penjelasan. Halaman petunjuk memberi penjelasan dan cara memastikan sertifikatnya asli (sidik jari di layar NAS) sebelum peringatan muncul.
- **HSTS ditolak.** Dengan HSTS, browser tidak lagi memberi pilihan untuk melewati peringatan sertifikat. Kalau sertifikat self-signed berganti (diperbarui, nama host diubah, atau data panel direset), admin terkunci dari panelnya sendiri sampai menghapus status HSTS di browser. HSTS baru aman setelah ada sertifikat tepercaya (Fase 2).
- Sertifikat dibuat oleh `sapo-api`, bukan `sapod`, karena tidak butuh hak root (ADR 0002).

## Alternatif yang dipertimbangkan

| Alternatif | Alasan tidak dipilih |
|---|---|
| HTTP dan HTTPS berdampingan, pengalihan opsional (seperti NAS komersial) | Password tetap bisa lewat tanpa enkripsi bila pengguna memakai alamat HTTP |
| HTTPS saja tanpa HTTP | Pengguna yang mengetik alamat tanpa `https://` hanya melihat "tidak bisa terhubung" tanpa penjelasan |
| CA lokal yang dipasang di perangkat pengguna | Butuh langkah pemasangan di setiap perangkat; bisa ditambahkan nanti sebagai opsi |

## Konsekuensi

- Kunjungan pertama dari setiap browser menampilkan peringatan sertifikat sekali, dan lagi bila sertifikat diperbarui (sekitar setiap 2 tahun) atau nama host diubah.
- `make dev` memakai `sapo-api -no-tls` (HTTP saja, hanya untuk pengembangan).
- Uji boot ISO di CI memeriksa:
  - HTTP hanya melayani halaman petunjuk;
  - API berjalan lewat HTTPS;
  - sidik jari yang diumumkan panel sama dengan sertifikat yang dipakai server;
  - sidik jari tampil di konsol.
