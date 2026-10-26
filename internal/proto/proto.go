// Package proto mendefinisikan kontrak komunikasi antara sapo-api dan sapod.
//
// Protokolnya JSON per baris lewat unix socket lokal: satu Request per baris,
// dijawab satu Response per baris. sapod hanya mengenal operasi bernama yang
// terdaftar; tidak ada operasi untuk menjalankan perintah bebas (ADR 0002).
package proto

import (
	"encoding/json"
	"time"
)

// MaxMessageBytes adalah batas ukuran satu pesan. Pesan yang lebih besar ditolak.
const MaxMessageBytes = 1 << 20

// Request adalah satu permintaan operasi ke sapod.
type Request struct {
	ID     uint64          `json:"id"`
	Op     string          `json:"op"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response adalah jawaban sapod untuk satu Request.
type Response struct {
	ID     uint64          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error adalah kesalahan yang dikembalikan sapod. Code stabil dan bisa
// dipetakan ke pesan terjemahan di panel; Message untuk log dan pengembang.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Kode error standar.
const (
	ErrUnknownOp     = "unknown_op"
	ErrInvalidParams = "invalid_params"
	ErrInternal      = "internal"
	ErrBadRequest    = "bad_request"
	// ErrNotFound: objek yang diminta (misalnya disk) tidak ada.
	ErrNotFound = "not_found"
	// ErrUnavailable: alat sistem yang dibutuhkan tidak ada atau tidak bisa
	// dijalankan (misalnya mode pengembangan di luar Linux).
	ErrUnavailable = "unavailable"
	// ErrBusy: sapod sedang mengerjakan terlalu banyak permintaan sejenis.
	ErrBusy = "busy"
	// Penolakan operasi pool karena keadaan disk atau pool (bukan karena
	// parameter salah bentuk). Panel menampilkan pesan khusus per kode.
	ErrDiskChanged   = "disk_changed"   // disk yang dipilih sudah berbeda (dicabut, diganti)
	ErrDiskInUse     = "disk_in_use"    // disk sedang dipakai sistem, pool, RAID, atau LVM
	ErrDiskHasData   = "disk_has_data"  // disk berisi data dan penghapusannya belum dikonfirmasi
	ErrDiskRemovable = "disk_removable" // disk lepas-pasang tidak diizinkan untuk pool
	ErrPoolExists    = "pool_exists"
	ErrPoolBusy      = "pool_busy" // pool sedang resilver atau sibuk dengan operasi lain
	// ErrConflict: keadaan sistem bertentangan dengan permintaan (misalnya
	// nama akun sudah dipakai akun sistem lain).
	ErrConflict = "conflict"
)

// Nama operasi yang dikenal sapod saat ini.
const (
	OpSystemInfo = "system.info"
	// OpSystemStats membaca riwayat pemakaian CPU, memori, jaringan, dan
	// disk beberapa menit terakhir (Resource Monitor). Hanya membaca.
	OpSystemStats  = "system.stats"
	OpFeaturesList = "features.list"
	OpStorageDisks = "storage.disks"
	OpStorageSMART = "storage.smart"
	// OpStoragePools membaca pool ZFS beserta kesehatan dan status scrub.
	OpStoragePools = "storage.pools"
	// OpStoragePoolCreate membuat pool ZFS baru. Destruktif: isi disk yang
	// dipilih dihapus.
	OpStoragePoolCreate = "storage.pool_create"
	// OpStoragePoolScrub memulai atau menghentikan scrub pool.
	OpStoragePoolScrub = "storage.pool_scrub"
	// OpStoragePoolReplace mengganti satu disk pool dengan disk lain
	// (resilver). Destruktif untuk disk baru.
	OpStoragePoolReplace = "storage.pool_replace"
	// OpStoragePoolDestroy menghapus pool beserta seluruh isinya.
	OpStoragePoolDestroy = "storage.pool_destroy"
	// OpAccountsSync menyamakan akun dan grup sistem Sapo dengan daftar
	// pengguna dan grup di panel.
	OpAccountsSync = "accounts.sync"
	// OpSMBPassword menyamakan password Samba seorang pengguna.
	OpSMBPassword = "smb.set_password"
	// OpSharesApply menyiapkan shared folder (dataset, izin) dan menulis
	// ulang konfigurasi Samba.
	OpSharesApply = "shares.apply"
	// OpSharesDelete menghapus shared folder beserta isinya. Destruktif.
	OpSharesDelete = "shares.delete"
	// OpSharesUsage membaca ruang terpakai setiap shared folder.
	OpSharesUsage = "shares.usage"
	// OpConsolePanelAddress memperbarui alamat panel (dan sidik jari
	// sertifikat) di layar login konsol NAS.
	OpConsolePanelAddress = "console.panel_address"
	// OpTimeStatus membaca zona waktu, jam, dan status NTP; OpTimeZones
	// daftar zona waktu; OpTimeSet mengubah zona waktu dan NTP.
	OpTimeStatus = "time.status"
	OpTimeZones  = "time.zones"
	OpTimeSet    = "time.set"

	// App Center (ADR 0012).
	OpAppsCatalog = "apps.catalog"
	OpAppsList    = "apps.list"
	OpAppsInstall = "apps.install"
	OpAppsStart   = "apps.start"
	OpAppsStop    = "apps.stop"
	OpAppsRemove  = "apps.remove"

	// Backup shared folder ke pool lain (ADR 0017).
	OpBackupsList    = "backups.list"
	OpBackupsSet     = "backups.set"
	OpBackupsDelete  = "backups.delete"
	OpBackupsRun     = "backups.run"
	OpBackupsRestore = "backups.restore"
	OpBackupsClaim   = "backups.claim"

	// Backup ke NAS lain (ADR 0019). Penerima: aliran zfs receive lewat file
	// descriptor dari sapo-api (data dari NAS pengirim lewat HTTPS); pengirim:
	// aliran zfs send ke sapo-api. sapod sendiri tidak pernah ke jaringan.
	OpRBackupRecvOpen   = "rbackup.recv_open"
	OpRBackupRecvFinish = "rbackup.recv_finish"
	OpRBackupList       = "rbackup.list"
	OpRBackupDestroy    = "rbackup.destroy"
	OpRBackupRestore    = "rbackup.restore"
	OpRBackupSendOpen   = "rbackup.send_open"
	OpRBackupSendFinish = "rbackup.send_finish"
	OpRBackupSendClean  = "rbackup.send_cleanup"

	// Backup ke S3/cloud (ADR 0020): restic di unit systemd terkunci,
	// dijalankan sapod; kredensial hanya disimpan sapod.
	OpCloudList       = "cloud.list"
	OpCloudRepoAdd    = "cloud.repo_add"
	OpCloudRepoDelete = "cloud.repo_delete"
	OpCloudSnapshots  = "cloud.snapshots"
	OpCloudJobSet     = "cloud.job_set"
	OpCloudJobDelete  = "cloud.job_delete"
	OpCloudRun        = "cloud.run"
	OpCloudRestore    = "cloud.restore"
	OpCloudLs         = "cloud.ls"

	// Installer ke disk (ADR 0016).
	OpInstallStatus = "install.status"
	OpInstallStart  = "install.start"

	// Penyimpanan eksternal (USB, ADR 0015).
	OpExternalList    = "external.list"
	OpExternalMount   = "external.mount"
	OpExternalUnmount = "external.unmount"

	// Snapshot ZFS terjadwal (ADR 0014).
	OpSnapshotsList   = "snapshots.list"
	OpSnapshotsCreate = "snapshots.create"
	OpSnapshotsDelete = "snapshots.delete"

	// Sapo Drive (ADR 0013): folder pribadi setiap pengguna.
	OpHomesApply  = "homes.apply"
	OpHomesStatus = "homes.status"
	OpHomesDelete = "homes.delete"

	// Pembaruan sistem (ADR 0011): OpUpdatesCheck menjalankan apt-get update
	// dan mendaftar paket yang bisa diperbarui; OpUpdatesInstall memasangnya
	// di unit systemd terpisah (tetap berjalan bila sapod di-restart);
	// OpUpdatesStatus membaca keadaan dan log pemasangan.
	OpUpdatesCheck   = "updates.check"
	OpUpdatesInstall = "updates.install"
	OpUpdatesStatus  = "updates.status"

	// Operasi File Station (ADR 0010). Semua dijalankan sebagai akun Linux
	// pengguna (proses pembantu), jadi izinnya ditegakkan kernel (ACL).
	OpFilesList = "files.list"
	OpFilesStat = "files.stat"
	// OpFilesRestore menyalin file/folder dari snapshot (ADR 0014).
	OpFilesRestore = "files.restore"
	OpFilesMkdir   = "files.mkdir"
	OpFilesRename  = "files.rename"
	OpFilesDelete  = "files.delete"
	// OpFilesOpenRead dan OpFilesOpenWrite mengembalikan file descriptor
	// (SCM_RIGHTS) bersama jawaban; OpFilesCommit memberi nama akhir file
	// unggahan, OpFilesAbort membuangnya.
	OpFilesOpenRead  = "files.open_read"
	OpFilesOpenWrite = "files.open_write"
	OpFilesCommit    = "files.commit"
	OpFilesAbort     = "files.abort"

	// OpNetworkStatus membaca antarmuka jaringan, alamat, dan pengaturan
	// jaringan yang dikelola Sapo.
	OpNetworkStatus = "network.status"
	// OpNetworkApply menerapkan pengaturan jaringan baru. Harus dikonfirmasi
	// (OpNetworkConfirm) dalam batas waktu; bila tidak, pengaturan lama
	// dikembalikan otomatis.
	OpNetworkApply = "network.apply"
	// OpNetworkConfirm menyimpan pengaturan yang baru diterapkan.
	OpNetworkConfirm = "network.confirm"
	// OpNetworkCancel langsung mengembalikan pengaturan lama.
	OpNetworkCancel = "network.cancel"
)

// SystemInfo adalah hasil operasi system.info.
type SystemInfo struct {
	Hostname      string    `json:"hostname"`
	OS            string    `json:"os"`
	Kernel        string    `json:"kernel"`
	Arch          string    `json:"arch"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	CPU           CPUInfo   `json:"cpu"`
	Memory        MemInfo   `json:"memory"`
	LoadAverage   []float64 `json:"load_average"`
	SapoVersion   string    `json:"sapo_version"`
}

// CPUInfo merangkum prosesor.
type CPUInfo struct {
	Model string `json:"model"`
	Cores int    `json:"cores"`
}

// MemInfo dalam byte.
type MemInfo struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// Feature adalah ringkasan satu manifest fitur (lihat folder features/).
type Feature struct {
	ID         string            `json:"id" yaml:"id"`
	Name       map[string]string `json:"name" yaml:"name"`
	Group      string            `json:"group" yaml:"group"`
	Packages   []string          `json:"packages" yaml:"packages"`
	Services   []string          `json:"services" yaml:"services"`
	Requires   []string          `json:"requires" yaml:"requires"`
	Activation string            `json:"activation" yaml:"activation"`
	Removable  bool              `json:"removable" yaml:"removable"`
}

// ConsolePanelParams adalah parameter console.panel_address. Isi layar konsol
// dibuat sapod dari templat tetap; hanya nilai-nilai ini yang berasal dari
// sapo-api, dan semuanya divalidasi.
type ConsolePanelParams struct {
	Scheme      string `json:"scheme"`      // http atau https
	Port        int    `json:"port"`        // 1-65535
	Fingerprint string `json:"fingerprint"` // SHA-256 AB:CD:… (wajib untuk https)
}

// Disk adalah satu disk fisik (atau virtual) hasil operasi storage.disks.
type Disk struct {
	Name       string `json:"name"` // nama kernel, misalnya sda atau nvme0n1
	Path       string `json:"path"`
	SizeBytes  uint64 `json:"size_bytes"`
	Model      string `json:"model"`
	Vendor     string `json:"vendor"`
	Serial     string `json:"serial"`
	WWN        string `json:"wwn"`
	Transport  string `json:"transport"` // sata, sas, nvme, usb, virtio, …
	Rotational bool   `json:"rotational"`
	Removable  bool   `json:"removable"`
	// FSType dan PTType: tanda tangan sistem berkas dan tabel partisi di
	// disk itu sendiri (bukan di partisinya).
	FSType     string      `json:"fstype"`
	PTType     string      `json:"pttype"`
	Label      string      `json:"label"`
	Partitions []Partition `json:"partitions"`
	// Usage menentukan apakah disk boleh dipakai untuk pool baru:
	// DiskFree, DiskHasData (boleh, setelah penghapusan dikonfirmasi), atau
	// DiskInUse (tidak boleh; alasannya di UsageReason).
	Usage       string `json:"usage"`
	UsageReason string `json:"usage_reason,omitempty"`
	// Reasons: semua alasan pakai yang ditemukan (UsageReason hanya yang
	// terpenting). Hanya dipakai di dalam sapod.
	Reasons []string `json:"-"`
}

// Nilai Disk.Usage.
const (
	DiskFree    = "free"
	DiskHasData = "data"
	DiskInUse   = "in_use"
)

// Kode galat pemilihan disk tambahan (ADR 0015).
const (
	// ErrDiskUSB: disk USB tidak dipakai untuk pool kecuali diizinkan.
	ErrDiskUSB = "disk_usb"
	// ErrDiskIdentity: nomor seri atau WWN disk tidak unik (enclosure USB
	// murah), jadi disk tidak bisa dipastikan untuk operasi destruktif.
	ErrDiskIdentity = "disk_identity"
)

// Nilai Disk.UsageReason untuk DiskInUse.
const (
	UsageMounted = "mounted" // ada sistem berkas yang sedang terpasang (termasuk sistem Sapo)
	UsageSwap    = "swap"
	UsagePool    = "pool" // anggota pool ZFS
	UsageRAID    = "raid" // anggota RAID mdadm
	UsageLVM     = "lvm"
	UsageCrypt   = "crypt" // volume terenkripsi yang sedang terbuka
	// UsageHolder: dipegang perangkat lain yang tidak dikenali lebih rinci
	// (device-mapper, multipath, bcache, …). Gagal-tertutup: lebih baik
	// menolak disk daripada menghapus disk yang sedang dipakai.
	UsageHolder = "holder"
	// UsageZFSLabel (untuk DiskHasData): label ZFS dari pool yang tidak
	// sedang aktif di NAS ini, misalnya pool yang sudah dihapus atau disk
	// dari NAS lain. Boleh dipakai setelah penghapusan dikonfirmasi.
	UsageZFSLabel = "zfs_label"
)

// Partition adalah partisi atau perangkat turunan sebuah disk.
type Partition struct {
	Name        string   `json:"name"`
	SizeBytes   uint64   `json:"size_bytes"`
	Type        string   `json:"type"` // part, raid1, lvm, crypt, …
	FSType      string   `json:"fstype"`
	Label       string   `json:"label"`
	Mountpoints []string `json:"mountpoints"`
}

// SMARTParams adalah parameter operasi storage.smart.
type SMARTParams struct {
	Device string `json:"device"` // nama disk dari storage.disks, misalnya sda
}

// Status SMART sebuah disk.
const (
	SMARTPassed      = "passed"      // tes kesehatan keseluruhan lulus
	SMARTFailed      = "failed"      // disk melaporkan akan rusak: segera ganti
	SMARTWarning     = "warning"     // tanda awal kerusakan: cadangkan data, rencanakan penggantian
	SMARTStandby     = "standby"     // disk sedang tidur; tidak dibangunkan untuk dibaca
	SMARTUnsupported = "unsupported" // disk tidak mendukung SMART (misalnya disk virtual)
	SMARTUnavailable = "unavailable" // SMART tidak bisa dibaca (izin, alat tidak ada, waktu habis)
)

// SMART adalah hasil operasi storage.smart.
type SMART struct {
	Device       string      `json:"device"`
	Status       string      `json:"status"`
	Message      string      `json:"message,omitempty"`
	Protocol     string      `json:"protocol,omitempty"` // ATA, NVMe, SCSI
	TemperatureC *int        `json:"temperature_c,omitempty"`
	PowerOnHours *int        `json:"power_on_hours,omitempty"`
	Attributes   []SMARTAttr `json:"attributes"`
}

// SMARTAttr adalah satu atribut SMART (ATA) atau satu nilai log kesehatan (NVMe).
type SMARTAttr struct {
	ID        int    `json:"id,omitempty"`
	Name      string `json:"name"`
	Value     *int   `json:"value,omitempty"`
	Worst     *int   `json:"worst,omitempty"`
	Threshold *int   `json:"threshold,omitempty"`
	Raw       string `json:"raw"`
	// Failing: nilai sudah (atau pernah) melewati ambang batas pabrik.
	Failing bool `json:"failing"`
	// Warning: nilai mentah bukan nol pada atribut yang dipantau
	// (agent.storage.warn_*), misalnya sektor yang dipindahkan.
	Warning bool `json:"warning"`
}

// PoolsResult adalah hasil operasi storage.pools.
type PoolsResult struct {
	// Available: alat ZFS terpasang dan modul kernel termuat. Bila false,
	// Message menjelaskan sebabnya dan pool tidak bisa dibuat.
	Available bool   `json:"available"`
	Message   string `json:"message,omitempty"`
	// Layouts adalah susunan pool yang boleh dibuat (agent.storage.pools).
	Layouts []PoolLayout `json:"layouts"`
	// AllowRemovable: disk lepas-pasang (misalnya flashdisk) boleh dipakai.
	AllowRemovable bool   `json:"allow_removable"`
	Pools          []Pool `json:"pools"`
}

// Susunan pool. Artinya tetap di kode (sapod dan panel); konfigurasi hanya
// memilih yang tersedia dan batas jumlah disknya.
const (
	LayoutSingle = "single" // satu disk, tanpa redundansi
	LayoutMirror = "mirror" // semua disk berisi salinan yang sama
	LayoutRAIDZ1 = "raidz1" // tahan 1 disk rusak
	LayoutRAIDZ2 = "raidz2" // tahan 2 disk rusak
	LayoutRAIDZ3 = "raidz3" // tahan 3 disk rusak
)

// LayoutFloor adalah jumlah disk paling sedikit yang diterima ZFS untuk
// setiap susunan. Konfigurasi boleh menaikkan batas ini, tidak menurunkannya.
var LayoutFloor = map[string]int{
	LayoutSingle: 1,
	LayoutMirror: 2,
	LayoutRAIDZ1: 2,
	LayoutRAIDZ2: 3,
	LayoutRAIDZ3: 4,
}

// PoolLayout adalah satu susunan pool yang boleh dibuat.
type PoolLayout struct {
	ID       string `json:"id"`
	MinDisks int    `json:"min_disks"`
	MaxDisks int    `json:"max_disks"`
}

// Pool adalah satu pool ZFS.
type Pool struct {
	Name string `json:"name"`
	// Health: ONLINE, DEGRADED, FAULTED, OFFLINE, UNAVAIL, REMOVED, SUSPENDED.
	Health         string `json:"health"`
	SizeBytes      uint64 `json:"size_bytes"`
	AllocatedBytes uint64 `json:"allocated_bytes"`
	FreeBytes      uint64 `json:"free_bytes"`
	CapacityPct    int    `json:"capacity_pct"`
	// Layout dari vdev teratas: salah satu Layout*, atau "mixed".
	Layout string `json:"layout"`
	// Status dan Action: penjelasan ZFS bila pool bermasalah.
	Status     string    `json:"status,omitempty"`
	Action     string    `json:"action,omitempty"`
	ErrorCount uint64    `json:"error_count"`
	Vdevs      []Vdev    `json:"vdevs"`
	Scan       *PoolScan `json:"scan,omitempty"`
}

// Vdev adalah satu perangkat di pohon pool: grup (mirror, raidz) atau disk.
type Vdev struct {
	Name string `json:"name"`
	// GUID ZFS: penunjuk disk yang tidak ambigu, juga untuk disk yang hilang.
	GUID           string `json:"guid,omitempty"`
	Type           string `json:"type"` // mirror, raidz, disk, file, …
	State          string `json:"state"`
	Path           string `json:"path,omitempty"`
	Disk           string `json:"disk,omitempty"` // nama disk kernel (misalnya sdb), bila dikenali
	ReadErrors     uint64 `json:"read_errors"`
	WriteErrors    uint64 `json:"write_errors"`
	ChecksumErrors uint64 `json:"checksum_errors"`
	Children       []Vdev `json:"children,omitempty"`
}

// PoolScan adalah scrub atau resilver terakhir (atau yang sedang berjalan).
type PoolScan struct {
	Function  string `json:"function"` // scrub, resilver
	State     string `json:"state"`    // scanning, finished, canceled
	Paused    bool   `json:"paused"`
	StartTime int64  `json:"start_time"` // detik Unix
	EndTime   int64  `json:"end_time,omitempty"`
	Examined  uint64 `json:"examined_bytes"`
	ToExamine uint64 `json:"to_examine_bytes"`
	Issued    uint64 `json:"issued_bytes"`
	Errors    uint64 `json:"errors"`
}

// PoolCreateParams adalah parameter storage.pool_create.
type PoolCreateParams struct {
	Name   string    `json:"name"`
	Layout string    `json:"layout"`
	Disks  []DiskRef `json:"disks"`
	// AllowUSB: admin sengaja memakai disk USB (ADR 0015).
	AllowUSB bool `json:"allow_usb,omitempty"`
}

// DiskRef menunjuk disk yang dipilih pengguna, beserta identitas yang dilihat
// pengguna saat memilih. sapod menolak bila disk sekarang berbeda, jadi disk
// yang tertukar atau baru dicolok tidak pernah ikut terhapus.
type DiskRef struct {
	Name      string `json:"name"`
	Serial    string `json:"serial"`
	WWN       string `json:"wwn"`
	SizeBytes uint64 `json:"size_bytes"`
	// Usage: Disk.Usage yang dilihat pengguna. Bila berubah (misalnya disk
	// kosong kini berisi data), sapod menolak dengan ErrDiskChanged.
	Usage string `json:"usage"`
	// Erase: pengguna sudah mengonfirmasi bahwa data di disk ini dihapus.
	// Wajib untuk disk berstatus DiskHasData.
	Erase bool `json:"erase"`
}

// PoolScrubParams adalah parameter storage.pool_scrub.
type PoolScrubParams struct {
	Name   string `json:"name"`
	Action string `json:"action"` // start atau stop
}

// PoolReplaceParams adalah parameter storage.pool_replace.
type PoolReplaceParams struct {
	Name    string  `json:"name"`
	OldGUID string  `json:"old_guid"` // Vdev.GUID disk yang diganti
	Disk    DiskRef `json:"disk"`     // disk pengganti
	// AllowUSB: admin sengaja memakai disk USB (ADR 0015).
	AllowUSB bool `json:"allow_usb,omitempty"`
}

// PoolDestroyParams adalah parameter storage.pool_destroy.
type PoolDestroyParams struct {
	Name string `json:"name"`
}

// AccountsSyncParams adalah daftar lengkap pengguna dan grup Sapo.
type AccountsSyncParams struct {
	Users  []AccountUser `json:"users"`
	Groups []string      `json:"groups"`
}

// AccountUser adalah satu pengguna untuk accounts.sync.
type AccountUser struct {
	Name     string   `json:"name"`
	Groups   []string `json:"groups"`
	Disabled bool     `json:"disabled"`
}

// AccountsSyncResult melaporkan perubahan. Conflicts berisi "user:nama" atau
// "group:nama" yang tidak bisa dibuat karena nama sudah dipakai akun atau
// grup sistem di luar Sapo.
type AccountsSyncResult struct {
	// Failures: akun atau grup yang gagal diubah ("user:nama: pesan");
	// sisanya tetap diterapkan.
	Failures      []string `json:"failures"`
	CreatedUsers  []string `json:"created_users,omitempty"`
	RemovedUsers  []string `json:"removed_users,omitempty"`
	CreatedGroups []string `json:"created_groups,omitempty"`
	RemovedGroups []string `json:"removed_groups,omitempty"`
	Conflicts     []string `json:"conflicts"`
}

// MaxSMBPassword: panjang password paling banyak (byte) yang bisa dipakai
// untuk SMB; panel menolak password lebih panjang.
const MaxSMBPassword = 127

// SMBPasswordParams adalah parameter smb.set_password.
type SMBPasswordParams struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// SharesApplyParams adalah daftar lengkap shared folder. SMBOff/NFSOff
// mematikan layanan itu (Control Panel); nilai kosong = menyala.
type SharesApplyParams struct {
	Shares []ShareSpec `json:"shares"`
	SMBOff bool        `json:"smb_off,omitempty"`
	NFSOff bool        `json:"nfs_off,omitempty"`
	// HomesPool: folder pribadi (Sapo Drive) di pool ini juga dibagikan
	// lewat SMB ([homes]); kosong = tidak.
	HomesPool string `json:"homes_pool,omitempty"`
	// ExternalOff: folder eksternal (USB) tidak dibagikan lewat SMB
	// (gagal-tertutup saat akun tidak bisa disinkronkan).
	ExternalOff bool `json:"external_off,omitempty"`
}

// ShareSpec adalah satu shared folder: dataset <Pool>/<Name>. Read dan Write
// berisi "user:nama" atau "group:nama"; selain itu tidak punya akses.
type ShareSpec struct {
	Name        string   `json:"name"`
	Pool        string   `json:"pool"`
	Description string   `json:"description"`
	Read        []string `json:"read"`
	Write       []string `json:"write"`
	// NFS: folder juga diekspor lewat NFS (nil = tidak).
	NFS *NFSExport `json:"nfs,omitempty"`
	// Apps: aplikasi App Center yang diberi akses ke folder (ADR 0012).
	Apps []ShareApp `json:"apps,omitempty"`
	// Snapshots: jadwal snapshot folder (nil = mati; snapshot yang sudah ada
	// tidak dihapus).
	Snapshots *SnapshotPolicy `json:"snapshots,omitempty"`
}

// SnapshotPolicy adalah jadwal snapshot satu dataset: berapa snapshot per
// jam, harian, dan mingguan yang disimpan (0 = jenis itu tidak diambil), dan
// jam (waktu lokal NAS) snapshot harian dan mingguan (Senin) diambil.
type SnapshotPolicy struct {
	Hourly int `json:"hourly"`
	Daily  int `json:"daily"`
	Weekly int `json:"weekly"`
	Hour   int `json:"hour"`
}

// Batas jadwal snapshot.
const (
	MaxSnapshotHourly = 168
	MaxSnapshotDaily  = 366
	MaxSnapshotWeekly = 520
)

// Valid: angka dalam batas.
func (p SnapshotPolicy) Valid() bool {
	return p.Hourly >= 0 && p.Hourly <= MaxSnapshotHourly && p.Daily >= 0 && p.Daily <= MaxSnapshotDaily &&
		p.Weekly >= 0 && p.Weekly <= MaxSnapshotWeekly && p.Hour >= 0 && p.Hour <= 23
}

// SnapshotTarget menunjuk dataset: shared folder Share di Pool, atau folder
// pribadi User (Sapo Drive) bila Share kosong.
type SnapshotTarget struct {
	Pool  string `json:"pool"`
	Share string `json:"share,omitempty"`
	User  string `json:"user,omitempty"`
}

// SnapshotInfo adalah satu snapshot. Kind: hourly, daily, weekly, manual.
type SnapshotInfo struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Created   time.Time `json:"created"`
	UsedBytes uint64    `json:"used_bytes"`
}

// SnapshotsListResult: snapshot dataset (terbaru dulu) dan ruang yang hanya
// dipakai snapshot.
type SnapshotsListResult struct {
	Snapshots       []SnapshotInfo `json:"snapshots"`
	UsedBySnapshots uint64         `json:"used_by_snapshots"`
}

// SnapshotParams menunjuk satu snapshot (snapshots.delete).
type SnapshotParams struct {
	SnapshotTarget
	Name string `json:"name"`
}

// ShareApp: aplikasi ID boleh membaca folder (dan menulis bila Write).
// Folder dipasang di dalam container aplikasi, dan akun aplikasinya
// mendapat entri ACL sendiri; Samba tidak memakainya.
type ShareApp struct {
	ID    string `json:"id"`
	Write bool   `json:"write,omitempty"`
}

// MaxNFSClients membatasi jumlah jaringan klien NFS per folder.
const MaxNFSClients = 32

// NFSExport adalah ekspor NFS satu folder. Semua akses dari klien dipetakan
// ke satu akun Sapo (User), jadi izin NFS sama dengan izin akun itu.
// Clients berisi alamat atau jaringan dalam bentuk kanonis (netip), misalnya
// "192.168.1.0/24" atau "192.168.1.20/32". ReadOnly mengikuti izin efektif
// akun itu di folder (dihitung sapo-api, termasuk lewat grup).
type NFSExport struct {
	Clients  []string `json:"clients"`
	User     string   `json:"user"`
	ReadOnly bool     `json:"read_only"`
}

// SharesApplyResult melaporkan keadaan setiap folder, Samba, dan NFS.
type SharesApplyResult struct {
	Shares []ShareState `json:"shares"`
	SMB    string       `json:"smb"`
	// NFS: "applied", atau "unavailable" bila server NFS belum terpasang.
	NFS string `json:"nfs"`
}

// ShareState adalah hasil satu folder; Error kosong bila siap.
type ShareState struct {
	Name  string `json:"name"`
	Pool  string `json:"pool"`
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
	// NFSError: folder siap, tetapi ekspor NFS-nya tidak dipasang.
	NFSError string `json:"nfs_error,omitempty"`
	// SnapshotError: folder siap, tetapi jadwal snapshot-nya gagal diatur.
	SnapshotError string `json:"snapshot_error,omitempty"`
}

// ShareDeleteParams adalah parameter shares.delete.
type ShareDeleteParams struct {
	Name string `json:"name"`
	Pool string `json:"pool"`
}

// ShareUsage adalah ruang dataset folder (hasil shares.usage, per
// "<pool>/<folder>").
type ShareUsage struct {
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	// ACLPending: izin sedang diterapkan ke isi folder; ACLError: gagal.
	ACLPending bool   `json:"acl_pending,omitempty"`
	ACLError   string `json:"acl_error,omitempty"`
}

// NetworkConfig adalah pengaturan jaringan yang dikelola Sapo.
type NetworkConfig struct {
	Hostname   string             `json:"hostname"`
	Interfaces []NetworkInterface `json:"interfaces"`
	// DNS: server DNS bila semua antarmuka berupa alamat tetap (dengan DHCP,
	// DNS dari router).
	DNS []string `json:"dns"`
}

// NetworkInterface adalah pengaturan satu antarmuka fisik. IPv6 selalu
// otomatis (SLAAC).
type NetworkInterface struct {
	Name string `json:"name"`
	// Mode: "dhcp" atau "static".
	Mode string `json:"mode"`
	// Address: IPv4 dengan prefiks, misalnya "192.168.1.10/24" (static).
	Address string `json:"address,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

// NetworkStatus adalah hasil network.status.
type NetworkStatus struct {
	Hostname string `json:"hostname"`
	// Config: pengaturan yang terakhir dikonfirmasi; nil bila file jaringan
	// tidak (lagi) dibuat Sapo, misalnya setelah reset dari menu darurat.
	Config     *NetworkConfig `json:"config,omitempty"`
	Interfaces []NetworkLink  `json:"interfaces"`
	Gateway    string         `json:"gateway,omitempty"`
	DNS        []string       `json:"dns"`
	// Pending: perubahan yang menunggu konfirmasi.
	Pending *NetworkPending `json:"pending,omitempty"`
}

// NetworkLink adalah keadaan nyata satu antarmuka fisik.
type NetworkLink struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac"`
	Up        bool     `json:"up"`
	SpeedMbps int      `json:"speed_mbps,omitempty"`
	Addresses []string `json:"addresses"`
}

// NetworkPending: perubahan yang belum dikonfirmasi.
type NetworkPending struct {
	ConfirmBy time.Time     `json:"confirm_by"`
	Config    NetworkConfig `json:"config"`
}

// NetworkApplyParams adalah parameter network.apply.
type NetworkApplyParams struct {
	Config NetworkConfig `json:"config"`
}

// NetworkApplyResult: Token dipakai untuk konfirmasi atau pembatalan,
// AppliesAt adalah saat pengaturan baru mulai berlaku.
type NetworkApplyResult struct {
	Token     string    `json:"token"`
	AppliesAt time.Time `json:"applies_at"`
	ConfirmBy time.Time `json:"confirm_by"`
}

// NetworkTokenParams adalah parameter network.confirm dan network.cancel.
type NetworkTokenParams struct {
	Token string `json:"token"`
}

// StatsParams: hanya sampel dengan nomor urut setelah After (0 = dari
// awal riwayat). Paling banyak MaxStatsSamples per jawaban; ambil lagi
// dengan After = nomor terakhir sampai kurang dari itu.
type StatsParams struct {
	After uint64 `json:"after,omitempty"`
}

// MaxStatsSamples membatasi sampel per jawaban (batas ukuran pesan sapod).
const MaxStatsSamples = 120

// SystemStats adalah hasil system.stats: sampel urut dari yang terlama.
type SystemStats struct {
	IntervalSeconds float64       `json:"interval_seconds"`
	CPUCores        int           `json:"cpu_cores"`
	Samples         []StatsSample `json:"samples"`
}

// StatsSample adalah pemakaian sistem rata-rata selama satu selang.
type StatsSample struct {
	// Seq: nomor urut sampel (tidak bergantung pada jam, yang bisa mundur).
	Seq        uint64    `json:"seq"`
	Time       time.Time `json:"time"`
	CPUPercent float64   `json:"cpu_percent"`
	// IOWaitPercent: waktu CPU menunggu disk.
	IOWaitPercent    float64     `json:"iowait_percent"`
	MemoryUsedBytes  uint64      `json:"memory_used_bytes"`
	MemoryTotalBytes uint64      `json:"memory_total_bytes"`
	Net              []StatsRate `json:"net"`
	Disk             []StatsRate `json:"disk"`
}

// StatsRate: laju per detik satu antarmuka (diterima/dikirim) atau disk
// (dibaca/ditulis), dalam byte.
type StatsRate struct {
	Name string  `json:"name"`
	In   float64 `json:"in"`
	Out  float64 `json:"out"`
}

// FilesTarget menunjuk satu path di shared folder, atas nama satu pengguna.
// Path relatif terhadap akar folder, dipisah "/", tanpa "." atau "..".
type FilesTarget struct {
	User  string `json:"user"`
	Pool  string `json:"pool"`
	Share string `json:"share"`
	Path  string `json:"path"`
	// Home: folder pribadi User di Pool (Share kosong).
	Home bool `json:"home,omitempty"`
	// External: folder eksternal (USB) bernama Share; Pool kosong.
	External bool `json:"external,omitempty"`
	// Snapshot: dibaca dari snapshot ini (baca saja; list, stat, open_read),
	// atau sumber files.restore.
	Snapshot string `json:"snapshot,omitempty"`
}

// FilesRestoreParams: Path di snapshot FilesTarget.Snapshot disalin kembali
// ke tempat yang sama di folder sekarang. Bila nama itu sudah ada, hasilnya
// diberi nama baru, kecuali Overwrite dan keduanya file biasa.
type FilesRestoreParams struct {
	FilesTarget
	Overwrite bool `json:"overwrite,omitempty"`
}

// FilesRestoreResult: nama hasil pemulihan (di direktori yang sama).
type FilesRestoreResult struct {
	Name  string `json:"name"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	// Skipped: symlink dan berkas khusus yang tidak dipulihkan.
	Skipped int `json:"skipped,omitempty"`
}

// FilesRenameParams: To adalah path baru di folder yang sama.
type FilesRenameParams struct {
	FilesTarget
	To string `json:"to"`
	// Overwrite: menimpa file biasa di To secara atomik (WebDAV MOVE).
	Overwrite bool `json:"overwrite,omitempty"`
}

// FilesCommitParams: Temp dari files.open_write; Path adalah nama akhir.
type FilesCommitParams struct {
	FilesTarget
	Temp      string `json:"temp"`
	Overwrite bool   `json:"overwrite"`
}

// FileEntry adalah satu isi direktori.
type FileEntry struct {
	Name string `json:"name"`
	// Type: "dir", "file", "symlink", atau "other".
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// FilesListResult adalah hasil files.list.
type FilesListResult struct {
	Entries []FileEntry `json:"entries"`
	// Truncated: direktori berisi lebih banyak dari batas yang ditampilkan.
	Truncated bool `json:"truncated,omitempty"`
}

// FilesOpenResult menyertai file descriptor dari files.open_read/write.
type FilesOpenResult struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	// Temp: nama file sementara unggahan (files.open_write).
	Temp string `json:"temp,omitempty"`
}

// TimeStatus adalah hasil time.status.
type TimeStatus struct {
	Timezone string    `json:"timezone"`
	Now      time.Time `json:"now"`
	// NTP: sinkronisasi waktu otomatis menyala; Synchronized: jam sudah
	// tersinkron.
	NTP          bool `json:"ntp"`
	Synchronized bool `json:"synchronized"`
}

// TimeSetParams adalah parameter time.set.
type TimeSetParams struct {
	Timezone string `json:"timezone"`
	NTP      bool   `json:"ntp"`
}

// UpdatePackage adalah satu paket yang bisa diperbarui.
type UpdatePackage struct {
	Name string `json:"name"`
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

// UpdatesCheckResult adalah hasil updates.check.
type UpdatesCheckResult struct {
	CheckedAt time.Time       `json:"checked_at"`
	Packages  []UpdatePackage `json:"packages"`
}

// UpdatesStatus adalah hasil updates.status.
type UpdatesStatus struct {
	// Running: pemasangan sedang berjalan.
	Running bool `json:"running"`
	// Last: hasil pemasangan terakhir (nil bila belum pernah).
	Last *UpdateRun `json:"last,omitempty"`
	// Log: baris terakhir log pemasangan.
	Log []string `json:"log"`
}

// UpdateRun adalah hasil satu pemasangan pembaruan.
type UpdateRun struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	OK         bool      `json:"ok"`
	ExitCode   int       `json:"exit_code"`
	// Interrupted: pemasangan berhenti sebelum selesai (diisi updates.status).
	Interrupted bool `json:"interrupted,omitempty"`
}

// CatalogApp adalah satu aplikasi di katalog App Center.
type CatalogApp struct {
	ID          string            `json:"id"`
	Name        map[string]string `json:"name"`
	Description map[string]string `json:"description"`
	Website     string            `json:"website,omitempty"`
	Image       string            `json:"image"`
	DefaultPort int               `json:"default_port"`
	MemoryMB    int               `json:"memory_mb"`
	// SharePath: shared folder yang diberikan dipasang di
	// <SharePath>/<nama folder> di dalam container (kosong = aplikasi tidak
	// memakai shared folder). ShareWrite: aplikasi boleh diberi izin tulis.
	SharePath  string `json:"share_path,omitempty"`
	ShareWrite bool   `json:"share_write,omitempty"`
}

// InstalledApp adalah aplikasi yang terpasang (atau datanya masih ada).
type InstalledApp struct {
	ID   string `json:"id"`
	Pool string `json:"pool"`
	Port int    `json:"port"`
	// Installed: false bila aplikasi dihapus tetapi datanya disimpan.
	Installed bool `json:"installed"`
	// Enabled: dijalankan (juga saat boot); Status dari systemd: active,
	// activating, inactive, failed.
	Enabled     bool      `json:"enabled"`
	Status      string    `json:"status,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
	// Shares: shared folder yang sedang dipasang di container aplikasi.
	Shares []AppShare `json:"shares,omitempty"`
}

// AppShare: shared folder Name dipasang di container aplikasi (bisa ditulis
// bila Write).
type AppShare struct {
	Name  string `json:"name"`
	Write bool   `json:"write,omitempty"`
}

// AppParams menunjuk satu aplikasi.
type AppParams struct {
	ID string `json:"id"`
}

// AppInstallParams adalah parameter apps.install.
type AppInstallParams struct {
	ID   string `json:"id"`
	Pool string `json:"pool"`
	Port int    `json:"port"`
}

// AppRemoveParams: RemoveData menghapus dataset data aplikasi juga.
type AppRemoveParams struct {
	ID         string `json:"id"`
	RemoveData bool   `json:"remove_data"`
}

// HomesApplyParams adalah keadaan lengkap folder pribadi (Sapo Drive):
// setiap pengguna di Users mendapat dataset <Pool>/sapo-home/<nama>.
// Folder pengguna yang tidak ada di daftar TIDAK dihapus.
type HomesApplyParams struct {
	Pool  string     `json:"pool"`
	Users []HomeUser `json:"users"`
	// Snapshots: jadwal snapshot semua folder pribadi (nil = mati).
	Snapshots *SnapshotPolicy `json:"snapshots,omitempty"`
}

// HomeUser: QuotaBytes 0 = tanpa batas.
type HomeUser struct {
	Name       string `json:"name"`
	QuotaBytes uint64 `json:"quota_bytes"`
}

// HomesApplyResult: Failures berisi "nama: alasan" untuk folder yang gagal
// disiapkan.
type HomesApplyResult struct {
	Failures []string `json:"failures"`
}

// HomeStatus adalah satu folder pribadi yang ada di sistem (di pool mana
// pun).
type HomeStatus struct {
	Pool           string `json:"pool"`
	User           string `json:"user"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	QuotaBytes     uint64 `json:"quota_bytes"`
}

// HomeDeleteParams menghapus folder pribadi pengguna yang sudah dihapus.
type HomeDeleteParams struct {
	Pool string `json:"pool"`
	User string `json:"user"`
}

// ExternalVolume adalah satu sistem berkas di drive USB (disk tanpa partisi
// atau satu partisi).
type ExternalVolume struct {
	UUID      string `json:"uuid"`
	Device    string `json:"device"` // nama kernel, misalnya sdb1
	Disk      string `json:"disk"`   // disk induk, misalnya sdb
	Model     string `json:"model"`
	Serial    string `json:"serial"`
	FSType    string `json:"fstype"`
	Label     string `json:"label"`
	SizeBytes uint64 `json:"size_bytes"`
	// Supported: format yang bisa dipasang (exFAT, FAT, NTFS). Reason: alasan
	// tidak bisa dipasang (format lain, anggota pool, sedang dipakai sistem).
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	// Diisi bila sudah diatur sebagai folder eksternal.
	Name     string `json:"name,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
	Group    string `json:"group,omitempty"`
	SMB      bool   `json:"smb,omitempty"`
	// State: available, mounted, disconnected (diatur tetapi drive tidak
	// tercolok), error.
	State          string `json:"state"`
	Error          string `json:"error,omitempty"`
	UsedBytes      uint64 `json:"used_bytes,omitempty"`
	AvailableBytes uint64 `json:"available_bytes,omitempty"`
}

// ExternalMountParams memasang volume UUID sebagai folder eksternal Name.
// Group: grup Sapo yang boleh membuka (sapo-users = semua pengguna).
type ExternalMountParams struct {
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	ReadOnly bool   `json:"read_only"`
	Group    string `json:"group"`
	SMB      bool   `json:"smb"`
}

// ExternalListParams: Cached = hanya folder yang terpasang dari pemeriksaan
// terakhir (murah, untuk File Station), tanpa membaca ulang drive.
type ExternalListParams struct {
	Cached bool `json:"cached,omitempty"`
}

// ExternalParams menunjuk folder eksternal.
type ExternalParams struct {
	Name string `json:"name"`
}

// InstallStartParams memasang Sapo ke disk sistem (disk dihapus).
type InstallStartParams struct {
	Disk DiskRef `json:"disk"`
}

// InstallStatus: Live = berjalan dari ISO (installer tersedia).
type InstallStatus struct {
	Live    bool        `json:"live"`
	Running bool        `json:"running"`
	Last    *InstallRun `json:"last,omitempty"`
	Log     []string    `json:"log"`
	// MinDiskBytes: ukuran disk sistem paling kecil.
	MinDiskBytes uint64 `json:"min_disk_bytes"`
}

// InstallRun adalah hasil pemasangan terakhir. Step: langkah yang sedang
// atau terakhir dijalankan.
type InstallRun struct {
	Disk        string    `json:"disk"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	OK          bool      `json:"ok"`
	Step        string    `json:"step"`
	Error       string    `json:"error,omitempty"`
	Interrupted bool      `json:"interrupted,omitempty"`
}

// BackupJob adalah satu tugas backup: shared folder Share di SrcPool disalin
// ke DestPool setiap hari pukul Hour (waktu lokal NAS); Keep salinan
// disimpan di tujuan.
type BackupJob struct {
	ID       string `json:"id"`
	SrcPool  string `json:"src_pool"`
	Share    string `json:"share"`
	DestPool string `json:"dest_pool"`
	Hour     int    `json:"hour"`
	Keep     int    `json:"keep"`
	Enabled  bool   `json:"enabled"`
}

// BackupStatus: tugas, hasil terakhir, dan salinan di tujuan.
type BackupStatus struct {
	BackupJob
	Running    bool         `json:"running"`
	LastRun    time.Time    `json:"last_run,omitempty"`
	LastOK     bool         `json:"last_ok"`
	LastError  string       `json:"last_error,omitempty"`
	LastBytes  uint64       `json:"last_bytes,omitempty"`
	Copies     []BackupCopy `json:"copies"`
	UsedBytes  uint64       `json:"used_bytes"`
	DestExists bool         `json:"dest_exists"`
}

// BackupCopy adalah satu salinan (snapshot) di tujuan.
type BackupCopy struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

// BackupsListResult: tugas backup dan pemulihan yang sedang/terakhir
// berjalan.
type BackupsListResult struct {
	Jobs     []BackupStatus  `json:"jobs"`
	Restores []BackupRestore `json:"restores"`
}

// BackupRestore: pemulihan salinan Copy dari tugas JobID menjadi shared
// folder baru Name di Pool.
type BackupRestore struct {
	JobID    string    `json:"job_id"`
	Copy     string    `json:"copy"`
	Pool     string    `json:"pool"`
	Name     string    `json:"name"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Running  bool      `json:"running"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	// Present: dataset Pool/Name masih ada sebagai shared folder Name.
	Present bool `json:"present"`
	// Claimed: sapo-api sudah mendaftarkan folder ini (backups.claim, sekali
	// saja).
	Claimed bool `json:"claimed"`
}

// BackupClaimParams: klaim pendaftaran folder pulihan Pool/Name.
type BackupClaimParams struct {
	Pool string `json:"pool"`
	Name string `json:"name"`
}

// BackupDeleteParams: DeleteData juga menghapus salinan di tujuan.
type BackupDeleteParams struct {
	ID         string `json:"id"`
	DeleteData bool   `json:"delete_data"`
}

// BackupRestoreParams memulihkan salinan Copy tugas ID menjadi shared folder
// baru Name (di pool sumber).
type BackupRestoreParams struct {
	ID   string `json:"id"`
	Copy string `json:"copy"`
	Name string `json:"name"`
}

// RBackupTarget: salinan tugas Job dari partner Partner di pool penerima
// Pool (<pool>/sapo-backup-remote/<partner>/<job>). Job kosong di
// rbackup.destroy = semua salinan partner itu.
type RBackupTarget struct {
	Pool    string `json:"pool"`
	Partner string `json:"partner"`
	Job     string `json:"job"`
}

// RBackupRecvParams membuka aliran zfs receive: Base kosong = salinan
// pertama (penuh), selain itu harus snapshot terbaru di penerima.
type RBackupRecvParams struct {
	RBackupTarget
	Base string `json:"base"`
	Snap string `json:"snap"`
	// QuotaBytes: batas ruang semua salinan partner (0 = tanpa batas);
	// diatur admin penerima.
	QuotaBytes uint64 `json:"quota_bytes,omitempty"`
}

// RBackupSession: aliran yang sedang terbuka (file descriptor-nya dikirim
// bersama jawaban).
type RBackupSession struct {
	Session string `json:"session"`
	Snap    string `json:"snap,omitempty"`
	Base    string `json:"base,omitempty"`
}

// RBackupRecvFinish menutup aliran penerima. OK: data dari pengirim sampai
// utuh (bila tidak, zfs receive dihentikan). KeepDays: salinan yang
// DITERIMA kurang dari sekian hari lalu (jam penerima) tidak pernah dibuang,
// jadi NAS pengirim yang dibobol tidak bisa menggusur salinan lama dengan
// mengirim berulang-ulang.
type RBackupRecvFinish struct {
	Session  string `json:"session"`
	OK       bool   `json:"ok"`
	KeepDays int    `json:"keep_days"`
}

// RBackupList: salinan di penerima. Untuk Job kosong: Jobs berisi semua
// tugas partner itu.
type RBackupList struct {
	Copies    []BackupCopy     `json:"copies"`
	UsedBytes uint64           `json:"used_bytes"`
	Jobs      []RBackupJobInfo `json:"jobs,omitempty"`
}

// RBackupJobInfo: satu tugas partner di penerima.
type RBackupJobInfo struct {
	Job       string       `json:"job"`
	Copies    []BackupCopy `json:"copies"`
	UsedBytes uint64       `json:"used_bytes"`
}

// RBackupRestoreParams memulihkan salinan Copy menjadi shared folder baru
// Name di pool penerima.
type RBackupRestoreParams struct {
	RBackupTarget
	Copy string `json:"copy"`
	Name string `json:"name"`
}

// RBackupSendParams membuka aliran zfs send untuk tugas Job dari shared
// folder Pool/Share. Remote: nama salinan yang sudah ada di penerima.
type RBackupSendParams struct {
	Pool   string   `json:"pool"`
	Share  string   `json:"share"`
	Job    string   `json:"job"`
	Remote []string `json:"remote"`
}

// RBackupSendFinish menutup aliran pengirim. RemoteHas: penerima sudah
// menyimpan Snap (snapshot itu menjadi dasar berikutnya, tidak dibuang).
type RBackupSendFinish struct {
	Session   string `json:"session"`
	OK        bool   `json:"ok"`
	RemoteHas bool   `json:"remote_has"`
}

// CloudRepoParams menambah repo restic di S3. Existing: menyambung ke repo
// yang sudah ada (Password wajib, misalnya NAS pengganti); selain itu repo
// baru dibuat dan password acak dikembalikan sekali.
type CloudRepoParams struct {
	Name         string `json:"name"`
	Endpoint     string `json:"endpoint"` // host[:port]
	Bucket       string `json:"bucket"`
	Prefix       string `json:"prefix,omitempty"`
	Region       string `json:"region,omitempty"`
	InsecureHTTP bool   `json:"insecure_http,omitempty"`
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	Existing     bool   `json:"existing,omitempty"`
	Password     string `json:"password,omitempty"`
}

// CloudRepo: repo tanpa rahasia (untuk panel).
type CloudRepo struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Endpoint     string    `json:"endpoint"`
	Bucket       string    `json:"bucket"`
	Prefix       string    `json:"prefix,omitempty"`
	Region       string    `json:"region,omitempty"`
	InsecureHTTP bool      `json:"insecure_http,omitempty"`
	Created      time.Time `json:"created"`
}

// CloudRepoAdded: hasil cloud.repo_add. Password hanya untuk repo baru.
type CloudRepoAdded struct {
	ID       string `json:"id"`
	Password string `json:"password,omitempty"`
}

// CloudJob: satu shared folder ke satu repo, harian; salinan KeepDays hari
// terakhir disimpan (selalu paling sedikit satu).
type CloudJob struct {
	ID       string `json:"id"`
	Repo     string `json:"repo"`
	Pool     string `json:"pool"`
	Share    string `json:"share"`
	Hour     int    `json:"hour"`
	KeepDays int    `json:"keep_days"`
	Enabled  bool   `json:"enabled"`
}

// CloudJobStatus: tugas dan hasil terakhirnya.
type CloudJobStatus struct {
	CloudJob
	Running   bool      `json:"running"`
	LastRun   time.Time `json:"last_run,omitempty"`
	LastOK    bool      `json:"last_ok"`
	LastError string    `json:"last_error,omitempty"`
	LastAdded uint64    `json:"last_added_bytes,omitempty"`
	LastTotal uint64    `json:"last_total_bytes,omitempty"`
}

// CloudListResult: repo, tugas, dan apakah restic terpasang.
type CloudListResult struct {
	Available bool             `json:"available"`
	Repos     []CloudRepo      `json:"repos"`
	Jobs      []CloudJobStatus `json:"jobs"`
}

// CloudSnapshot: satu salinan restic di repo (termasuk dari NAS lain/lama).
type CloudSnapshot struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Share    string    `json:"share,omitempty"`
	Job      string    `json:"job,omitempty"`
	Path     string    `json:"path"`
}

// CloudRestoreParams memulihkan salinan Snapshot dari Repo menjadi shared
// folder baru Pool/Name.
type CloudRestoreParams struct {
	Repo     string `json:"repo"`
	Snapshot string `json:"snapshot"`
	Pool     string `json:"pool"`
	Name     string `json:"name"`
	// Path: file/folder di dalam salinan (relatif terhadap akarnya) yang
	// dipulihkan saja; kosong = seluruh salinan.
	Path string `json:"path,omitempty"`
}

// CloudLsParams menelusuri satu folder (Path relatif, kosong = akar) di
// dalam salinan Snapshot.
type CloudLsParams struct {
	Repo     string `json:"repo"`
	Snapshot string `json:"snapshot"`
	Path     string `json:"path"`
}

// CloudEntry: satu isi folder di salinan. Type: dir, file, symlink, other.
type CloudEntry struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"`
	Size  uint64    `json:"size"`
	MTime time.Time `json:"mtime"`
}

// CloudLsResult: isi folder; Truncated bila lebih dari batas daftar.
type CloudLsResult struct {
	Entries   []CloudEntry `json:"entries"`
	Truncated bool         `json:"truncated"`
}

// CloudIDParams: satu repo atau tugas.
type CloudIDParams struct {
	ID string `json:"id"`
}
