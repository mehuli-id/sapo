package auth

import (
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewRecoveryCodesFormat(t *testing.T) {
	codes, hashes, err := NewRecoveryCodes(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 10 || len(hashes) != 10 {
		t.Fatalf("jumlah %d/%d", len(codes), len(hashes))
	}
	re := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{5}-[0-9A-HJKMNP-TV-Z]{5}$`)
	seen := map[string]bool{}
	for i, c := range codes {
		if !re.MatchString(c) {
			t.Errorf("format kode %q", c)
		}
		if seen[c] {
			t.Errorf("kode ganda %q", c)
		}
		seen[c] = true
		// Hash tidak memuat kode asli.
		if strings.Contains(hashes[i], strings.ReplaceAll(c, "-", "")) {
			t.Error("hash tidak boleh memuat kode")
		}
	}
}

func TestRecoveryCodeMatchingIsForgiving(t *testing.T) {
	codes, hashes, _ := NewRecoveryCodes(1)
	c := codes[0]
	plain := strings.ReplaceAll(c, "-", "")
	// Salin tanpa tanda hubung, huruf kecil, spasi, dan O/I/L yang tertukar.
	variants := []string{c, plain, strings.ToLower(c), " " + c[:5] + " " + c[6:] + " ",
		strings.NewReplacer("0", "O", "1", "I").Replace(c)}
	for _, v := range variants {
		if matchRecovery(hashes, v) != 0 {
			t.Errorf("varian %q harus cocok dengan %q", v, c)
		}
	}
	for _, v := range []string{"", "123456", plain[:9], plain + "0", "UUUUU-UUUUU"} {
		if matchRecovery(hashes, v) != -1 {
			t.Errorf("%q tidak boleh cocok", v)
		}
	}
	if LooksLikeRecoveryCode("123456") || !LooksLikeRecoveryCode(c) {
		t.Fatal("kode TOTP 6 digit dan kode cadangan harus bisa dibedakan")
	}
}

func TestConsumeRecoveryCodeOnce(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	codes, hashes, _ := NewRecoveryCodes(3)
	if err := s.CompleteSetup(User{Username: "arthur", Role: RoleAdmin, RecoveryCodes: hashes}, "id", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Dua login bersamaan dengan kode yang sama: hanya satu berhasil.
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ConsumeRecoveryCode("arthur", codes[0]); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("kode yang sama berhasil %d kali, harus 1", ok)
	}
	left, err := s.ConsumeRecoveryCode("arthur", codes[1])
	if err != nil || left != 1 {
		t.Fatalf("sisa %d err %v", left, err)
	}
	// Kode baru menggantikan semua kode lama.
	_, fresh, _ := NewRecoveryCodes(2)
	if err := s.SetRecoveryCodes("arthur", fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeRecoveryCode("arthur", codes[2]); err != ErrRecoveryInvalid {
		t.Fatalf("kode lama harus tidak berlaku, dapat %v", err)
	}
	// Tersimpan di disk: dibuka ulang, jumlahnya tetap.
	s2, _ := OpenStore(s.path[:len(s.path)-len("/users.json")])
	if u, _ := s2.Get("arthur"); len(u.RecoveryCodes) != 2 {
		t.Fatalf("setelah dibuka ulang ada %d kode", len(u.RecoveryCodes))
	}
}
