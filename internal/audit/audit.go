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

// Log menulis ke <dir>/audit.log (izin 0600, mode append). Saat ukurannya
// melewati batas, file dirotasi ke audit.log.1 (satu arsip), agar banjir
// percobaan login tidak bisa memenuhi disk sistem.
type Log struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	size    int64
	maxSize int64
}

// Open membuka file audit dengan batas ukuran maxBytes (0 = tanpa batas).
func Open(dir string, maxBytes int64) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Log{path: filepath.Join(dir, "audit.log"), maxSize: maxBytes}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

func (l *Log) rotateLocked() error {
	if err := l.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return err
	}
	return l.open()
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
	if l.maxSize > 0 && l.size+int64(len(b))+1 > l.maxSize {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	}
	n, err := l.f.Write(append(b, '\n'))
	l.size += int64(n)
	return err
}

// Close menutup file.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	return l.f.Close()
}
