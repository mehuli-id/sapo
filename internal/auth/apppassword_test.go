package auth

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestAppPasswords(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	if err := st.CompleteSetup(User{Username: "budi", Role: RoleAdmin, PasswordHash: "x"}, "id", now); err != nil {
		t.Fatal(err)
	}
	secret, ap, err := NewAppPassword("Laptop", now)
	if err != nil || !regexp.MustCompile(`^[a-z]{4}(-[a-z]{4}){3}$`).MatchString(secret) || strings.Contains(ap.Hash, secret) {
		t.Fatalf("password aplikasi = %q %+v %v", secret, ap, err)
	}
	if err := st.AddAppPassword("budi", ap); err != nil {
		t.Fatal(err)
	}
	// Huruf besar, spasi, dan tanpa tanda hubung tetap cocok.
	for _, in := range []string{secret, strings.ToUpper(secret), strings.ReplaceAll(secret, "-", " ")} {
		if id, ok := st.CheckAppPassword("budi", in, now); !ok || id != ap.ID {
			t.Errorf("%q harus cocok", in)
		}
	}
	if _, ok := st.CheckAppPassword("budi", "salah", now); ok {
		t.Fatal("password salah cocok")
	}
	if _, ok := st.CheckAppPassword("ani", secret, now); ok {
		t.Fatal("akun lain cocok")
	}
	if u, _ := st.Get("budi"); u.AppPasswords[0].LastUsedAt == nil {
		t.Fatal("waktu pakai harus dicatat")
	}
	// Password akun diganti: semua password aplikasi dicabut.
	if err := st.SetPassword("budi", "y", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.CheckAppPassword("budi", secret, now); ok {
		t.Fatal("password aplikasi harus dicabut saat password diganti")
	}
	// Dicabut satu per satu, dan dibatasi jumlahnya.
	_, ap2, _ := NewAppPassword("HP", now)
	_ = st.AddAppPassword("budi", ap2)
	if name, err := st.RevokeAppPassword("budi", ap2.ID); err != nil || name != "HP" {
		t.Fatalf("cabut: %q %v", name, err)
	}
	if _, err := st.RevokeAppPassword("budi", ap2.ID); err != ErrAppPasswordNotFound {
		t.Fatalf("cabut dua kali: %v", err)
	}
	for i := 0; i < MaxAppPasswords; i++ {
		_, a, _ := NewAppPassword("x", now)
		if err := st.AddAppPassword("budi", a); err != nil {
			t.Fatal(err)
		}
	}
	_, a, _ := NewAppPassword("x", now)
	if err := st.AddAppPassword("budi", a); err != ErrAppPasswordLimit {
		t.Fatalf("batas: %v", err)
	}
	if ValidAppPasswordName("") || ValidAppPasswordName("a\nb") || !ValidAppPasswordName("Laptop kantor") {
		t.Fatal("validasi nama")
	}
}
