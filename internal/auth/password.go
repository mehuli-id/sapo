// Package auth berisi penyimpanan user, hash password, TOTP, dan sesi untuk
// sapo-api. Semua angka kebijakan (panjang minimal, parameter Argon2, batas
// percobaan login) datang dari konfigurasi, bukan ditulis di sini.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params adalah parameter Argon2id.
type Argon2Params struct {
	MemoryKiB   uint32 `yaml:"memory_kib"`
	Iterations  uint32 `yaml:"iterations"`
	Parallelism uint8  `yaml:"parallelism"`
}

const (
	saltLen = 16
	keyLen  = 32
)

var b64 = base64.RawStdEncoding

// HashPassword menghasilkan hash Argon2id dalam format PHC:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
func HashPassword(password string, p Argon2Params) (string, error) {
	if p.MemoryKiB == 0 || p.Iterations == 0 || p.Parallelism == 0 {
		return "", errors.New("parameter argon2 tidak valid")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword membandingkan password dengan hash secara constant-time.
// Parameter dibaca dari hash, jadi hash lama tetap bisa diverifikasi setelah
// parameter di konfigurasi dinaikkan.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("format hash tidak dikenal")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("versi argon2 tidak didukung")
	}
	var p Argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Iterations, &p.Parallelism); err != nil {
		return false, errors.New("parameter hash tidak valid")
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash benar bila hash dibuat dengan parameter yang berbeda dari p.
func NeedsRehash(encoded string, p Argon2Params) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return true
	}
	return parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", p.MemoryKiB, p.Iterations, p.Parallelism)
}
