// Package proto mendefinisikan kontrak komunikasi antara sapo-api dan sapod.
//
// Protokolnya JSON per baris lewat unix socket lokal: satu Request per baris,
// dijawab satu Response per baris. sapod hanya mengenal operasi bernama yang
// terdaftar; tidak ada operasi untuk menjalankan perintah bebas (ADR 0002).
package proto

import "encoding/json"

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
)

// Nama operasi yang dikenal sapod saat ini.
const (
	OpSystemInfo   = "system.info"
	OpFeaturesList = "features.list"
	OpStorageDisks = "storage.disks"
	OpStorageSMART = "storage.smart"
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

// Disk adalah satu disk fisik (atau virtual) hasil operasi storage.disks.
type Disk struct {
	Name       string      `json:"name"` // nama kernel, misalnya sda atau nvme0n1
	Path       string      `json:"path"`
	SizeBytes  uint64      `json:"size_bytes"`
	Model      string      `json:"model"`
	Vendor     string      `json:"vendor"`
	Serial     string      `json:"serial"`
	WWN        string      `json:"wwn"`
	Transport  string      `json:"transport"` // sata, sas, nvme, usb, virtio, …
	Rotational bool        `json:"rotational"`
	Removable  bool        `json:"removable"`
	Partitions []Partition `json:"partitions"`
}

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
