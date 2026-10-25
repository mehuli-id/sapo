# ADR 0019: Backup ke NAS Sapo lain

- **Status:** Diterima
- **Tanggal:** 2026-10-25

## Konteks

Backup ke pool lain (ADR 0017) masih berada di lokasi yang sama: kebakaran, pencurian, atau banjir menghilangkan NAS dan backup-nya sekaligus. Pengguna butuh salinan di NAS Sapo lain (rumah lain, kantor cabang), diatur dari panel, tanpa membuka akses shell ke NAS mana pun.

## Keputusan

- **Tanpa SSH.** Sapo tidak menjalankan sshd, dan membukanya menambah permukaan serangan. Data dibawa lewat HTTPS sapo-api yang sudah ada:
  - di pengirim, `sapod` menjalankan `zfs send` dan memberi sapo-api ujung baca pipa lewat file descriptor (SCM_RIGHTS, seperti File Station);
  - sapo-api mengalirkannya ke NAS tujuan;
  - di penerima, sapo-api menulis ke ujung pipa `zfs receive` yang dibuka `sapod`.
  - `sapod` tetap tidak pernah ke jaringan (ADR 0002).
- **Pemasangan.** Di NAS penerima, admin membuat *partner*: nama, pool, lama simpan (hari), dan kuota. Panel lalu menampilkan sekali kode `SAPO1-<partner>-<token>-<32 heks sidik jari sertifikat penerima>`; penerima hanya menyimpan hash SHA-256 token. Di NAS pengirim, admin memasukkan alamat dan kode. Sapo-api pengirim hanya mau bicara dengan server yang sidik jari sertifikatnya berawalan sidik jari di kode, lalu mengunci sidik jari penuhnya (`VerifyConnection`, bukan CA). Bila sertifikat penerima berganti, backup berhenti sampai dipasangkan ulang.
- **Wewenang partner sengaja sempit.** API partner (`/api/v1/rbackup/`, token per permintaan, tanpa cookie sehingga tanpa CSRF, kegagalan dibatasi per IP) hanya bisa: `hello`, membaca daftar salinan miliknya, dan mengirim salinan baru ke `<pool>/sapo-backup-remote/<partner>/<tugas>`.
  - Partner **tidak bisa** menghapus salinan atau mengatur lama simpan.
  - **Retensi berdasarkan waktu terima** (jam penerima, properti `sapo:received`, bukan waktu di stream yang bisa dipalsukan; dicatat untuk setiap snapshot yang jadi walau alirannya dilaporkan gagal, dan snapshot tanpa catatan dianggap baru; urutan salinan menurut `createtxg`). Salinan yang diterima kurang dari N hari lalu tidak pernah dibuang, dan penerimaan dibatasi 4 per tugas per 24 jam. NAS pengirim yang dibobol (misalnya ransomware) tidak bisa menggusur salinan baik dengan mengirim berulang-ulang; admin punya N hari untuk menyadarinya.
  - **Stream dari partner tidak dipercaya.** `sapod` membaca rekaman DRR_BEGIN pertama sebelum menjalankan `zfs receive`. Yang ditolak: stream gabungan (`zfs send -R`/`-p`, yang bisa membawa dataset anak dan properti seperti `mountpoint`, `setuid`, `sharenfs`), clone, raw/redacted, zvol, dan jenis penuh/inkremental yang tidak sesuai permintaan. Lapis kedua: setiap penerimaan memakai `-o canmount=noauto -o readonly=on -x mountpoint -x sharenfs -x sharesmb -x setuid -x exec -x devices`; dataset induk partner `setuid/devices/exec=off`. Target `ds@snap`: nama snapshot ditentukan penerima. Header dengan payload atau stream lanjutan (resume) juga ditolak. Risiko sisa: bug OpenZFS pada stream yang dibuat jahat.
  - `sapod` penerima memeriksa nama pool/partner/tugas/snapshot dengan pola tetap. Penerimaan penuh hanya ke dataset yang belum ada; inkremental hanya dari snapshot terbaru di penerima (tanpa `receive -F`).
  - Kuota per partner (`quota` ZFS di dataset partner; gagal memasangnya = tidak menerima), paling banyak 50 tugas per partner, satu penerimaan per partner pada satu waktu, sesi terima dan kirim dihitung terpisah.
  - Salinan tidak dipasang, `readonly`, dan bertanda `sapo:backup=remote:<partner>/<tugas>`.
  - Partner yang dicabut tanpa menghapus salinan tetap tampil (kode tidak berlaku); salinannya bisa dipulihkan atau dihapus nanti.
- **Data dulu.**
  - Folder asal tidak pernah diubah oleh backup.
  - Dasar inkremental yang hilang di sumber membuat backup berhenti (`backup_diverged`).
  - Snapshot yang belum tentu diterima dibuang; yang sudah diterima (dicek ulang lewat daftar salinan) tetap menjadi dasar berikutnya.
  - Shared folder yang dipakai tugas tidak bisa dihapus.
  - Pool penerima yang menyimpan salinan partner tidak bisa dihapus sebelum partner dihapus.
  - Menghapus partner beserta salinan butuh ketik ulang nama, password, dan log audit (`destructive`).
- **Pemulihan di penerima.** Salinan dipulihkan sebagai shared folder baru di NAS penerima, tanpa izin pengguna; admin memberi izin setelahnya. Ini untuk saat NAS asal rusak. Mengirim balik ke NAS asal menyusul.
- **Jadwal** harian di sapo-api pengirim (waktu NAS), satu pengiriman pada satu waktu, percobaan gagal diulang paling cepat sejam kemudian. Kegagalan masuk notifikasi email `backup_failed` (ADR 0018); hasil terakhir tampil di aplikasi Backup.

## Akibat

- NAS tujuan harus bisa dihubungi di port HTTPS panel dan alamat pengirim harus masuk `api.allowed_networks` penerima (bawaan: jaringan lokal). Untuk lokasi lain, pakai VPN; membuka panel ke internet tidak disarankan.
- Token partner tersimpan di pengirim (`backup-remote.json`, 0600) karena harus dikirim di setiap permintaan; panel tidak pernah menampilkannya lagi.
- Uji asap ISO memakai satu VM sebagai pengirim dan penerima (`127.0.0.1:5001`): pemasangan (kode dengan sidik jari lain ditolak), kirim penuh dan inkremental, pemulihan di penerima, baca lewat SMB, dan penolakan hapus pool penerima.
