package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
)

// Kode cadangan 2FA: kode sekali pakai untuk masuk bila aplikasi autentikator
// tidak tersedia (misalnya HP hilang). Formatnya XXXXX-XXXXX dari alfabet
// Crockford base32 (tanpa I, L, O, U), 50 bit acak per kode. Karena acak dan
// panjang, kode disimpan sebagai SHA-256 (bukan Argon2); tebakan tetap dibatasi
// oleh penghitung kode 2FA salah per akun.

const recoveryAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const recoveryLen = 10

// ErrRecoveryInvalid: kode cadangan salah atau sudah dipakai.
var ErrRecoveryInvalid = errors.New("kode cadangan salah atau sudah dipakai")

// NewRecoveryCodes membuat n kode cadangan. Kode asli hanya ditampilkan
// sekali kepada pengguna; yang disimpan hanya hash-nya.
func NewRecoveryCodes(n int) (codes, hashes []string, err error) {
	buf := make([]byte, recoveryLen)
	for i := 0; i < n; i++ {
		if _, err := rand.Read(buf); err != nil {
			return nil, nil, err
		}
		var b strings.Builder
		for j, v := range buf {
			if j == recoveryLen/2 {
				b.WriteByte('-')
			}
			// 256 habis dibagi 32: tidak ada bias.
			b.WriteByte(recoveryAlphabet[int(v)%len(recoveryAlphabet)])
		}
		code := b.String()
		codes = append(codes, code)
		hashes = append(hashes, hashRecovery(normalizeRecovery(code)))
	}
	return codes, hashes, nil
}

// LooksLikeRecoveryCode membedakan kode cadangan dari kode TOTP 6 digit.
func LooksLikeRecoveryCode(code string) bool {
	return len(normalizeRecovery(code)) == recoveryLen
}

// normalizeRecovery: huruf besar, tanpa spasi atau tanda hubung, dan huruf
// yang mudah tertukar dipetakan seperti aturan Crockford (O→0, I/L→1).
// Hasil kosong bila ada karakter di luar alfabet.
func normalizeRecovery(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch {
		case r == '-' || r == ' ':
			continue
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		}
		if !strings.ContainsRune(recoveryAlphabet, r) {
			return ""
		}
		b.WriteRune(r)
	}
	return b.String()
}

func hashRecovery(normalized string) string {
	sum := sha256.Sum256([]byte("sapo-recovery:" + normalized))
	return hex.EncodeToString(sum[:])
}

// matchRecovery mengembalikan indeks hash yang cocok dengan code, atau -1.
// Semua hash dibandingkan (waktu tidak bergantung pada posisi yang cocok).
func matchRecovery(hashes []string, code string) int {
	n := normalizeRecovery(code)
	if len(n) != recoveryLen {
		return -1
	}
	want := []byte(hashRecovery(n))
	found := -1
	for i, h := range hashes {
		if subtle.ConstantTimeCompare([]byte(h), want) == 1 {
			found = i
		}
	}
	return found
}
