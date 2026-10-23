// Package audit mencatat setiap kejadian penting (setup, login, perubahan
// sistem) sebagai baris JSON di file yang hanya bisa ditambah dari aplikasi.
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event adalah satu catatan audit.
type Event struct {
	Time   time.Time         `json:"time"`
	Actor  string            `json:"actor,omitempty"`
	Action string            `json:"action"`
	IP     string            `json:"ip,omitempty"`
	OK     bool              `json:"ok"`
	Detail map[string]string `json:"detail,omitempty"`
}

// Log menulis ke <dir>/audit.log (izin 0600, mode append).
type Log struct {
	mu sync.Mutex
	f  *os.File
}

// Open membuka file audit.
func Open(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "audit.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f}, nil
}

// Record menulis satu kejadian. Kegagalan menulis dikembalikan agar
// pemanggil bisa memutuskan (misalnya menolak operasi berisiko).
func (l *Log) Record(e Event) error {
	if l == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.f.Write(append(b, '\n'))
	return err
}

// Close menutup file.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	return l.f.Close()
}
