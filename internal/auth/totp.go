package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP sesuai RFC 6238 (SHA-1, 6 digit, periode 30 detik) — format yang
// didukung semua aplikasi autentikator (Google Authenticator, Aegis, dll.).
const (
	totpPeriod = 30
	totpDigits = 6
	// Toleransi jam: kode dari satu periode sebelum/sesudah masih diterima.
	totpSkew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret membuat rahasia acak 160 bit (base32).
func NewTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// TOTPURI menyusun URI otpauth:// untuk QR code.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func totpCode(key []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, v%mod)
}

// VerifyTOTP memeriksa kode dan mengembalikan langkah waktu (step) yang cocok.
// lastStep adalah step terakhir yang pernah dipakai user; kode dari step yang
// sama atau lebih lama ditolak agar tidak bisa dipakai ulang (replay).
func VerifyTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		step := cur + d
		if step <= lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(totpCode(key, step)), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// TOTPCodeAt menghasilkan kode untuk waktu t (dipakai test dan alat bantu).
func TOTPCodeAt(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", err
	}
	return totpCode(key, t.Unix()/totpPeriod), nil
}
