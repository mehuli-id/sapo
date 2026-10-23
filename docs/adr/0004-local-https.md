# ADR 0004: HTTPS wajib di jaringan lokal dengan sertifikat self-signed

- **Status:** Diterima
- **Tanggal:** 2026-10-24

## Konteks

Sampai ADR ini, panel dan password admin dikirim lewat HTTP tanpa enkripsi di LAN (ADR 0003). Dokumen rancangan menetapkan "HTTPS wajib, HSTS, TLS 1.2 ke atas" untuk mencegah penyadapan, misalnya dari Wi-Fi yang sama.

Sertifikat tepercaya (Let's Encrypt) butuh domain dan baru tersedia lewat wizard "Akses dari internet" di Fase 2. Di jaringan lokal, NAS hanya bisa memakai sertifikat yang dibuatnya sendiri, sehingga browser menampilkan peringatan pada kunjungan pertama.

## Keputusan

- **`sapo-api` membuat sertifikat self-signed sendiri** (ECDSA P-256, tanpa root) di `<data_dir>/tls/`, dengan direktori 0700 dan kunci 0600. Isinya mencakup nama host, `<nama host>.local`, `localhost`, `api.hostnames`, dan alamat IP mesin saat dibuat. Masa berlakunya `api.tls.validity_days`: bawaan 397 hari, maksimal 825 (batas Apple), dihitung dari `NotBefore`.
- **Sertifikat dibuat ulang hanya bila** hilang atau rusak, mendekati kedaluwarsa (`api.tls.renew_before_days`), atau nama host berubah. Nama host baru terdeteksi saat `sapo-api` restart. Perubahan alamat IP dan jam yang mundur (papan tanpa RTC sebelum NTP sinkron) tidak memicu pembuatan ulang, agar sidik jari yang sudah dicocokkan pengguna tetap sama. Pemeriksaan berjalan saat start dan setiap hari, lalu sertifikat baru dipakai tanpa restart. Unit `sapo-api` diurutkan setelah `time-sync.target`.
- **HTTPS wajib** (`api.tls.enabled`, port `api.tls.listen`, bawaan 5001). Alamat HTTP (`api.listen`, bawaan 5000) hanya menyajikan:
  - file panel, yang menampilkan halaman petunjuk HTTPS;
  - `/api/v1/ui/config`, berisi port HTTPS dan sidik jari sertifikat;
  - `/api/v1/health`.

  Semua API lain lewat HTTP dijawab `403 https_required`, termasuk login dan setup. Jadi password tidak pernah dikirim lewat HTTP, walaupun ada yang memanggil API secara langsung.
- **Halaman petunjuk** menjelaskan peringatan sertifikat. Teksnya lewat i18n.
- **Sumber sidik jari yang dipercaya adalah layar login konsol NAS.**
  - Halaman petunjuk juga menampilkan sidik jari, tetapi hanya sebagai kemudahan, dan teksnya menyatakan demikian. Halaman itu datang lewat HTTP, sehingga bisa diubah penyerang aktif.
  - Layar konsol ditulis oleh `sapod` (root) lewat operasi bernama `console.panel_address`. `sapo-api` hanya mengirim skema, port, dan sidik jari, dan semuanya divalidasi ketat. Isi layar dibuat dari templat tetap di `/run/sapo/console.issue`, di direktori milik `sapod`, dengan penulisan atomik yang tidak mengikuti symlink.
  - Paket memasang tautan `/etc/issue.d/sapo.issue` ke file itu, lalu `sapod` menjalankan `agetty --reload` agar layar login yang sedang tampil ikut diperbarui.
  - `sapo-api` tidak pernah menulis file yang dibaca root (ADR 0002).
- **TLS 1.2 ke atas.** Lewat HTTPS, cookie sesi bernama `__Host-sapo_session`. Browser hanya menerima cookie berawalan ini bila `Secure`, `Path=/`, dan tanpa `Domain`. Ini perlu karena cookie tidak dipisah per port: tanpa awalan itu, cookie yang ditanam lewat HTTP di port 5000 akan ikut terkirim ke HTTPS.
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

- **Perlindungan penuh hanya terhadap penyadap pasif.** Penyerang aktif di jaringan yang sama (misalnya Wi-Fi bersama) tetap bisa mengubah halaman HTTP di port 5000: mengganti sidik jari dan tombolnya, atau memalsukan form login (sslstrip). HSTS atau sertifikat tepercaya baru menutup celah ini di Fase 2. Sampai saat itu:
  - pengguna diarahkan untuk mencocokkan sidik jari dengan layar NAS, bukan dengan halaman;
  - setelah HTTPS sekali dibuka, browser bisa langsung diarahkan ke `https://…:5001`, tanpa lewat HTTP lagi.
- Kunjungan pertama dari setiap browser menampilkan peringatan sertifikat sekali, dan lagi bila sertifikat diperbarui (bawaan sekitar setahun sekali) atau nama host diubah.
- `make dev` memakai `sapo-api -no-tls` (HTTP saja, hanya untuk pengembangan).
- Uji boot ISO di CI memeriksa:
  - HTTP hanya melayani halaman petunjuk;
  - API berjalan lewat HTTPS;
  - sidik jari yang diumumkan panel sama dengan sertifikat yang dipakai server;
  - sidik jari tampil di konsol.
