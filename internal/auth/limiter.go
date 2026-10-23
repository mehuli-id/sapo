package auth

import (
	"sync"
	"time"
)

// Limiter membatasi percobaan login per kunci (IP atau username).
//
// Setiap percobaan dipesan secara atomik dengan Begin SEBELUM password
// diperiksa, sehingga permintaan paralel tidak bisa melewati batas (semua
// percobaan dihitung, lalu dihapus dengan Reset bila login berhasil).
type Limiter struct {
	MaxAttempts int
	Window      time.Duration
	Lockout     time.Duration
	MaxEntries  int

	mu      sync.Mutex
	entries map[string]*limitEntry
	now     func() time.Time
}

type limitEntry struct {
	attempts    int
	first       time.Time
	lockedUntil time.Time
}

// NewLimiter membuat limiter dengan nilai dari konfigurasi.
func NewLimiter(maxAttempts int, window, lockout time.Duration) *Limiter {
	return &Limiter{MaxAttempts: maxAttempts, Window: window, Lockout: lockout, MaxEntries: 10000, entries: map[string]*limitEntry{}, now: time.Now}
}

// Begin memesan satu percobaan. Mengembalikan false (dan sisa waktu kunci)
// bila kunci sedang terkunci atau batas percobaan sudah habis.
func (l *Limiter) Begin(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if ok {
		if rem := e.lockedUntil.Sub(now); rem > 0 {
			return false, rem
		}
		if now.Sub(e.first) > l.Window {
			ok = false
		}
	}
	if !ok {
		l.evictLocked(now)
		e = &limitEntry{first: now}
		l.entries[key] = e
	}
	e.attempts++
	if e.attempts > l.MaxAttempts {
		e.lockedUntil = now.Add(l.Lockout)
		e.attempts = 0
		e.first = now
		return false, l.Lockout
	}
	return true, 0
}

// Locked mengembalikan sisa waktu kunci (0 bila tidak terkunci), tanpa
// memesan percobaan.
func (l *Limiter) Locked(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[key]; ok {
		if rem := e.lockedUntil.Sub(l.now()); rem > 0 {
			return rem
		}
	}
	return 0
}

// Reset menghapus catatan percobaan (setelah login berhasil).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// evictLocked menjaga ukuran peta tetap terbatas: hapus entri kedaluwarsa,
// lalu bila masih penuh, hapus entri tidak terkunci yang paling lama.
func (l *Limiter) evictLocked(now time.Time) {
	if len(l.entries) < l.MaxEntries {
		return
	}
	for k, e := range l.entries {
		if now.Sub(e.first) > l.Window && now.After(e.lockedUntil) {
			delete(l.entries, k)
		}
	}
	for len(l.entries) >= l.MaxEntries {
		var oldestKey string
		var oldest time.Time
		for k, e := range l.entries {
			if now.Before(e.lockedUntil) {
				continue
			}
			if oldestKey == "" || e.first.Before(oldest) {
				oldestKey, oldest = k, e.first
			}
		}
		if oldestKey == "" {
			return // semua terkunci; biarkan, jumlahnya dibatasi oleh lockout
		}
		delete(l.entries, oldestKey)
	}
}
