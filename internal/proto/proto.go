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
)

// Nama operasi yang dikenal sapod saat ini.
const (
	OpSystemInfo   = "system.info"
	OpFeaturesList = "features.list"
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
