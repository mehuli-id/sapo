package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Password aplikasi (Sapo Drive, ADR 0013): password khusus per perangkat
// untuk WebDAV, karena klien WebDAV tidak bisa memasukkan kode 2FA. Hanya
// berlaku untuk WebDAV (bukan login panel), bisa dicabut satu per satu, dan
// semuanya dicabut bila password akun diganti atau di-reset.
//
// Formatnya xxxx-xxxx-xxxx-xxxx dari 26 huruf kecil (sekitar 75 bit acak),
// mudah diketik di HP. Karena acak dan panjang, yang disimpan SHA-256 (bukan
// Argon2, yang terlalu berat untuk setiap permintaan WebDAV); tebakan tetap
// dibatasi limiter.

// AppPassword adalah satu password aplikasi.
type AppPassword struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Hash       string     `json:"hash"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// MaxAppPasswords membatasi jumlah password aplikasi per akun.
const MaxAppPasswords = 20

const (
	appPasswordGroups = 4
	appPasswordGroup  = 4
	appPasswordLetter = "abcdefghijklmnopqrstuvwxyz"
)

// Kesalahan password aplikasi.
var (
	ErrAppPasswordLimit    = errors.New("password aplikasi sudah terlalu banyak")
	ErrAppPasswordNotFound = errors.New("password aplikasi tidak ditemukan")
)

// ValidAppPasswordName: nama perangkat satu baris, 1–64 karakter.
func ValidAppPasswordName(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > 64 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return false
		}
	}
	return true
}

// NewAppPassword membuat password aplikasi baru. Password asli hanya
// ditampilkan sekali; yang disimpan hanya hash-nya.
func NewAppPassword(name string, now time.Time) (secret string, ap AppPassword, err error) {
	buf := make([]byte, appPasswordGroups*appPasswordGroup)
	if _, err := rand.Read(buf); err != nil {
		return "", AppPassword{}, err
	}
	var b strings.Builder
	for i, v := range buf {
		if i > 0 && i%appPasswordGroup == 0 {
			b.WriteByte('-')
		}
		// 256 % 26 ≠ 0: bias kecil (< 1%) tidak berarti untuk 75 bit.
		b.WriteByte(appPasswordLetter[int(v)%len(appPasswordLetter)])
	}
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return "", AppPassword{}, err
	}
	secret = b.String()
	return secret, AppPassword{ID: hex.EncodeToString(id), Name: name, Hash: appPasswordHash(secret), CreatedAt: now.UTC()}, nil
}

// appPasswordHash: spasi, tanda hubung, dan huruf besar diabaikan (diketik
// ulang di HP).
func appPasswordHash(secret string) string {
	norm := strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == ' ':
			return -1
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return r
	}, secret)
	sum := sha256.Sum256([]byte("sapo-app-password:" + norm))
	return hex.EncodeToString(sum[:])
}

// AddAppPassword menyimpan password aplikasi baru.
func (s *Store) AddAppPassword(username string, ap AppPassword) error {
	return s.updateUser(username, func(u *User) error {
		if len(u.AppPasswords) >= MaxAppPasswords {
			return ErrAppPasswordLimit
		}
		u.AppPasswords = append(append([]AppPassword(nil), u.AppPasswords...), ap)
		return nil
	})
}

// RevokeAppPassword mencabut satu password aplikasi; nama perangkatnya
// dikembalikan (untuk log audit).
func (s *Store) RevokeAppPassword(username, id string) (string, error) {
	name := ""
	err := s.updateUser(username, func(u *User) error {
		next := make([]AppPassword, 0, len(u.AppPasswords))
		for _, ap := range u.AppPasswords {
			if ap.ID == id {
				name = ap.Name
				continue
			}
			next = append(next, ap)
		}
		if name == "" {
			return ErrAppPasswordNotFound
		}
		u.AppPasswords = next
		return nil
	})
	return name, err
}

// appPasswordTouch: waktu pakai terakhir dicatat paling sering sekali per
// jam (agar tidak menulis file pengguna di setiap permintaan WebDAV).
const appPasswordTouch = time.Hour

// CheckAppPassword: secret cocok dengan salah satu password aplikasi akun
// itu. Mengembalikan ID-nya. Semua entri dibandingkan (waktu tetap).
func (s *Store) CheckAppPassword(username, secret string, now time.Time) (string, bool) {
	want := appPasswordHash(secret)
	u, ok := s.Get(username)
	if !ok {
		return "", false
	}
	match := -1
	for i, ap := range u.AppPasswords {
		if subtle.ConstantTimeCompare([]byte(ap.Hash), []byte(want)) == 1 {
			match = i
		}
	}
	if match < 0 {
		return "", false
	}
	ap := u.AppPasswords[match]
	if ap.LastUsedAt == nil || now.Sub(*ap.LastUsedAt) > appPasswordTouch {
		at := now.UTC()
		_ = s.updateUser(username, func(u *User) error {
			next := append([]AppPassword(nil), u.AppPasswords...)
			for i := range next {
				if next[i].ID == ap.ID {
					next[i].LastUsedAt = &at
				}
			}
			u.AppPasswords = next
			return nil
		})
	}
	return ap.ID, true
}
