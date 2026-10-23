package auth

import (
	"sort"
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
	// Protected menandai kunci yang tidak boleh dibuang saat peta penuh.
	Protected func(key string) bool

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

// Refund mengembalikan satu percobaan yang sudah dipesan, misalnya bila
// password ternyata benar atau server sedang sibuk. Tidak membuka kunci.
func (l *Limiter) Refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[key]; ok && e.attempts > 0 && !l.now().Before(e.lockedUntil) {
		e.attempts--
	}
}

// evictLocked menjaga ukuran peta tetap terbatas. Entri yang Protected
// (misalnya penghitung akun yang benar-benar ada) tidak pernah dibuang,
// sehingga banjir entri baru tidak bisa menghapus penghitung sebuah akun.
// Pembuangan dilakukan per batch (10%) agar biayanya tidak O(n) per entri.
func (l *Limiter) evictLocked(now time.Time) {
	if len(l.entries) < l.MaxEntries {
		return
	}
	type cand struct {
		key   string
		first time.Time
	}
	var cands []cand
	for k, e := range l.entries {
		if l.Protected != nil && l.Protected(k) {
			continue
		}
		if now.Sub(e.first) > l.Window && now.After(e.lockedUntil) {
			delete(l.entries, k)
			continue
		}
		if now.Before(e.lockedUntil) && len(l.entries) < 2*l.MaxEntries {
			// Entri terkunci disimpan sampai kuncinya habis, kecuali peta
			// sudah dua kali batas (batas keras memori).
			continue
		}
		cands = append(cands, cand{k, e.first})
	}
	target := l.MaxEntries - l.MaxEntries/10
	if len(l.entries) <= target {
		return
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].first.Before(cands[j].first) })
	for _, c := range cands {
		if len(l.entries) <= target {
			break
		}
		delete(l.entries, c.key)
	}
}
