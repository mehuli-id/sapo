package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testParams = Argon2Params{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1}

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := HashPassword("kata-sandi-panjang", testParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("format hash: %s", h)
	}
	if ok, _ := VerifyPassword("kata-sandi-panjang", h); !ok {
		t.Fatal("password benar ditolak")
	}
	if ok, _ := VerifyPassword("kata-sandi-salah", h); ok {
		t.Fatal("password salah diterima")
	}
	h2, _ := HashPassword("kata-sandi-panjang", testParams)
	if h == h2 {
		t.Fatal("salt tidak acak")
	}
}

// Vektor uji dari RFC 6238 lampiran B (SHA-1), dipotong ke 6 digit.
func TestTOTPRFC6238Vectors(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	cases := map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037"}
	for ts, want := range cases {
		step, ok := VerifyTOTP(secret, want, time.Unix(ts, 0), 0)
		if !ok {
			t.Errorf("t=%d kode %s ditolak", ts, want)
		}
		if step != ts/30 && step != ts/30-1 && step != ts/30+1 {
			t.Errorf("t=%d step %d", ts, step)
		}
	}
}

func TestTOTPReplayRejected(t *testing.T) {
	secret, _ := NewTOTPSecret()
	key, _ := b32.DecodeString(secret)
	now := time.Unix(1_800_000_000, 0)
	code := totpCode(key, now.Unix()/30)
	step, ok := VerifyTOTP(secret, code, now, 0)
	if !ok {
		t.Fatal("kode valid ditolak")
	}
	if _, ok := VerifyTOTP(secret, code, now, step); ok {
		t.Fatal("kode yang sama dipakai ulang harus ditolak")
	}
}

func TestStoreSetupOnceAndPersists(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.SetupRequired() {
		t.Fatal("store baru harus butuh setup")
	}
	admin := User{Username: "arthur", Role: RoleAdmin, PasswordHash: "x", CreatedAt: time.Now()}
	if err := s.CompleteSetup(admin, "id", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteSetup(User{Username: "penyusup", Role: RoleAdmin}, "en", time.Now()); err != ErrSetupDone {
		t.Fatalf("setup kedua harus ditolak, dapat %v", err)
	}
	st, err := os.Stat(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("izin file user %v, harus 0600", st.Mode().Perm())
	}
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.SetupRequired() {
		t.Fatal("status setup hilang setelah dibuka ulang")
	}
	if _, ok := s2.Get("arthur"); !ok {
		t.Fatal("admin tidak tersimpan")
	}
	if _, ok := s2.Get("penyusup"); ok {
		t.Fatal("user dari setup kedua tidak boleh ada")
	}
}

func TestSessionsExpire(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewSessions(10*time.Minute, time.Hour)
	s.now = func() time.Time { return now }
	tok, _ := s.Create("arthur", RoleAdmin)
	if _, ok := s.Get(tok); !ok {
		t.Fatal("sesi baru tidak berlaku")
	}
	now = now.Add(11 * time.Minute)
	if _, ok := s.Get(tok); ok {
		t.Fatal("sesi idle harus kedaluwarsa")
	}
	tok2, _ := s.Create("arthur", RoleAdmin)
	for i := 0; i < 7; i++ { // tetap aktif tiap 9 menit, tapi lewat batas maksimal 1 jam
		now = now.Add(9 * time.Minute)
		s.Get(tok2)
	}
	if _, ok := s.Get(tok2); ok {
		t.Fatal("sesi harus berakhir setelah batas maksimal")
	}
	tok3, _ := s.Create("arthur", RoleAdmin)
	s.Delete(tok3)
	if _, ok := s.Get(tok3); ok {
		t.Fatal("sesi yang dihapus masih berlaku")
	}
}

func TestLimiterReservesAttemptsAtomically(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := NewLimiter(3, 15*time.Minute, 10*time.Minute)
	l.now = func() time.Time { return now }
	allowed := 0
	for i := 0; i < 10; i++ { // seperti 10 permintaan paralel sebelum ada hasil
		if ok, _ := l.Begin("ip:1.2.3.4"); ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("hanya 3 percobaan yang boleh lolos, dapat %d", allowed)
	}
	if l.Locked("ip:1.2.3.4") == 0 {
		t.Fatal("harus terkunci")
	}
	now = now.Add(11 * time.Minute)
	if ok, _ := l.Begin("ip:1.2.3.4"); !ok {
		t.Fatal("kunci harus lepas setelah lockout")
	}
	l.Reset("ip:1.2.3.4")
}

func TestLimiterBoundedSize(t *testing.T) {
	l := NewLimiter(5, time.Minute, time.Minute)
	l.MaxEntries = 100
	for i := 0; i < 1000; i++ {
		l.Begin(fmt.Sprintf("ip:%d", i))
	}
	if len(l.entries) > 100 {
		t.Fatalf("peta limiter tumbuh tanpa batas: %d", len(l.entries))
	}
}

func TestLimiterFloodCannotEraseProtectedKey(t *testing.T) {
	l := NewLimiter(5, 15*time.Minute, 15*time.Minute)
	l.MaxEntries = 100
	l.Protected = func(k string) bool { return k == "user:arthur" }
	for i := 0; i < 4; i++ {
		l.Begin("user:arthur")
	}
	for i := 0; i < 5000; i++ {
		l.Begin(fmt.Sprintf("ip:%d", i))
	}
	l.Begin("user:arthur") // ke-5: masih boleh
	if ok, _ := l.Begin("user:arthur"); ok {
		t.Fatal("banjir entri tidak boleh menghapus penghitung akun")
	}
}

func TestLimiterRefund(t *testing.T) {
	l := NewLimiter(2, time.Minute, time.Minute)
	for i := 0; i < 10; i++ {
		l.Begin("ip:a")
		l.Refund("ip:a") // percobaan dengan password benar tidak dihitung
	}
	if ok, _ := l.Begin("ip:a"); !ok {
		t.Fatal("percobaan yang dikembalikan tidak boleh menghasilkan kunci")
	}
}

func TestTOTPStepCompareAndSet(t *testing.T) {
	s, _ := OpenStore(t.TempDir())
	_ = s.CompleteSetup(User{Username: "arthur", Role: RoleAdmin, TOTPSecret: "LAMA"}, "id", time.Now())
	if err := s.ConsumeTOTPStep("arthur", "LAMA", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeTOTPStep("arthur", "LAMA", 100); err != ErrTOTPReused {
		t.Fatalf("step sama harus ditolak, dapat %v", err)
	}
	if err := s.ConsumeTOTPStep("arthur", "LAMA", 99); err != ErrTOTPReused {
		t.Fatal("step mundur harus ditolak")
	}
	// Rahasia diganti di antara pemeriksaan kode dan penyimpanan step: kode
	// dari rahasia lama tidak boleh diterima.
	if err := s.SetTOTP("arthur", "BARU", 150); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeTOTPStep("arthur", "LAMA", 200); err != ErrTOTPReused {
		t.Fatalf("kode dari rahasia lama harus ditolak, dapat %v", err)
	}
	if err := s.ConsumeTOTPStep("arthur", "BARU", 200); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteUserSessionsExcept(t *testing.T) {
	ss := NewSessions(time.Hour, time.Hour)
	keep, _ := ss.Create("arthur", RoleAdmin)
	other, _ := ss.Create("arthur", RoleAdmin)
	someoneElse, _ := ss.Create("budi", RoleAdmin)
	if n := ss.DeleteUserExcept("arthur", keep); n != 1 {
		t.Fatalf("sesi yang diakhiri %d, harus 1", n)
	}
	if _, ok := ss.Get(keep); !ok {
		t.Fatal("sesi yang sedang dipakai harus tetap berlaku")
	}
	if _, ok := ss.Get(other); ok {
		t.Fatal("sesi lain milik user harus berakhir")
	}
	if _, ok := ss.Get(someoneElse); !ok {
		t.Fatal("sesi user lain tidak boleh tersentuh")
	}
}

func TestNeedsRehash(t *testing.T) {
	h, _ := HashPassword("x", testParams)
	if NeedsRehash(h, testParams) {
		t.Fatal("parameter sama tidak perlu rehash")
	}
	if !NeedsRehash(h, Argon2Params{MemoryKiB: 16 * 1024, Iterations: 1, Parallelism: 1}) {
		t.Fatal("parameter berbeda harus rehash")
	}
}

func TestStoreKeepsAnActiveAdmin(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	if err := s.CompleteSetup(User{Username: "arthur", Role: RoleAdmin}, "id", now); err != nil {
		t.Fatal(err)
	}
	user, disabled := RoleUser, true
	if err := s.UpdateUser("arthur", UserChange{Role: &user}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("menurunkan admin terakhir: %v", err)
	}
	if err := s.UpdateUser("arthur", UserChange{Disabled: &disabled}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("menonaktifkan admin terakhir: %v", err)
	}
	if err := s.DeleteUser("arthur"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("menghapus admin terakhir: %v", err)
	}
	// Ditolak = tidak ada yang berubah, termasuk di file.
	reopened, _ := OpenStore(filepath.Dir(s.path))
	if u, ok := reopened.Get("arthur"); !ok || u.Role != RoleAdmin || u.Disabled {
		t.Fatalf("admin berubah: %+v", u)
	}
	// Dengan admin kedua yang aktif, admin pertama boleh diturunkan.
	if err := s.CreateUser(User{Username: "sari", Role: RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser("arthur", UserChange{Role: &user}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreGroupMembership(t *testing.T) {
	s, _ := OpenStore(t.TempDir())
	_ = s.CompleteSetup(User{Username: "arthur", Role: RoleAdmin}, "id", time.Now())
	if err := s.CreateUser(User{Username: "budi", Role: RoleUser, Groups: []string{"keluarga"}}); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("grup belum ada: %v", err)
	}
	_ = s.CreateGroup(Group{Name: "keluarga"})
	if err := s.CreateGroup(Group{Name: "keluarga"}); !errors.Is(err, ErrGroupExists) {
		t.Fatalf("grup ganda: %v", err)
	}
	_ = s.CreateUser(User{Username: "budi", Role: RoleUser, Groups: []string{"keluarga", "keluarga"}})
	if u, _ := s.Get("budi"); len(u.Groups) != 1 {
		t.Fatalf("grup ganda harus disatukan: %v", u.Groups)
	}
	members := []string{"arthur"}
	_ = s.UpdateGroup("keluarga", nil, &members)
	if u, _ := s.Get("budi"); len(u.Groups) != 0 {
		t.Fatalf("budi harus keluar dari grup: %v", u.Groups)
	}
	if u, _ := s.Get("arthur"); len(u.Groups) != 1 {
		t.Fatalf("arthur harus masuk grup: %v", u.Groups)
	}
	bogus := []string{"tidak-ada"}
	if err := s.UpdateGroup("keluarga", nil, &bogus); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("anggota tidak dikenal: %v", err)
	}
}
