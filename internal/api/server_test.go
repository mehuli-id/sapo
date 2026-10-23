package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/audit"
	"github.com/mehuli-id/sapo/internal/auth"
	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/proto"
)

type fakeAgent struct {
	results map[string]any
	err     error
	// errs memberi error per operasi; params mencatat parameter terakhir.
	errs   map[string]error
	params map[string]any
}

func (f *fakeAgent) Call(_ context.Context, op string, params any, out any) error {
	if f.params == nil {
		f.params = map[string]any{}
	}
	f.params[op] = params
	if f.err != nil {
		return f.err
	}
	if err := f.errs[op]; err != nil {
		return err
	}
	res, ok := f.results[op]
	if !ok {
		return &proto.Error{Code: proto.ErrUnknownOp, Message: op}
	}
	if out == nil {
		return nil
	}
	raw, _ := json.Marshal(res)
	return json.Unmarshal(raw, out)
}

type harness struct {
	t      *testing.T
	s      *Server
	now    time.Time
	cookie *http.Cookie
	data   string
	// Kode cadangan yang diterima saat runSetup.
	recovery []string
}

func newHarness(t *testing.T, agent AgentCaller) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.API.WebDir = t.TempDir()
	cfg.API.BrandingFile = "../../branding.yaml"
	cfg.API.WizardsDir = "../../wizards"
	cfg.API.Hostnames = []string{"example.com"} // host bawaan httptest
	cfg.Auth.Argon2 = config.Argon2{MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1}
	if err := os.WriteFile(filepath.Join(cfg.API.WebDir, "index.html"), []byte("<!doctype html><title>Sapo</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, now: time.Unix(1_800_000_000, 0), data: t.TempDir()}
	store, err := auth.OpenStore(h.data)
	if err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(h.data, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	h.s, err = New(Options{Config: cfg, Agent: agent, Store: store, Audit: al, Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type reqOpt func(*http.Request)

func fromIP(ip string) reqOpt {
	return func(r *http.Request) { r.RemoteAddr = net.JoinHostPort(ip, "40000") }
}
func noCSRF() reqOpt         { return func(r *http.Request) { r.Header.Del(CSRFHeader) } }
func origin(o string) reqOpt { return func(r *http.Request) { r.Header.Set("Origin", o) } }

func (h *harness) do(method, path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.RemoteAddr = "192.168.1.20:40000"
	if isMutating(method) {
		r.Header.Set(CSRFHeader, "1")
	}
	if h.cookie != nil {
		r.AddCookie(h.cookie)
	}
	for _, o := range opts {
		o(r)
	}
	rec := httptest.NewRecorder()
	h.s.ServeHTTP(rec, r)
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie || c.Name == SecureSessionCookie {
			if c.MaxAge < 0 {
				h.cookie = nil
			} else {
				h.cookie = c
			}
		}
	}
	return rec
}

func errCode(rec *httptest.ResponseRecorder) string {
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

// runSetup menyelesaikan setup awal dan mengembalikan rahasia TOTP admin.
func (h *harness) runSetup() string {
	h.t.Helper()
	rec := h.do(http.MethodPost, "/api/v1/setup/totp", map[string]string{"username": "arthur"})
	if rec.Code != http.StatusOK {
		h.t.Fatalf("setup/totp %d %s", rec.Code, rec.Body)
	}
	var tr setupTOTPResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &tr)
	h.recovery = tr.RecoveryCodes
	if !strings.HasPrefix(tr.URI, "otpauth://totp/Sapo:arthur?") {
		h.t.Fatalf("uri %s", tr.URI)
	}
	code, _ := auth.TOTPCodeAt(tr.Secret, h.now)
	rec = h.do(http.MethodPost, "/api/v1/setup", setupRequest{
		Language: "id", Username: "Arthur", Password: "lumbung padi di bukit", PasswordConfirm: "lumbung padi di bukit", TOTPCode: code, TOTPToken: tr.Token,
	})
	if rec.Code != http.StatusOK {
		h.t.Fatalf("setup %d %s", rec.Code, rec.Body)
	}
	return tr.Secret
}

func TestProtectedEndpointsNeedLogin(t *testing.T) {
	h := newHarness(t, &fakeAgent{results: map[string]any{proto.OpSystemInfo: proto.SystemInfo{Hostname: "sapo-lab"}}})
	if rec := h.do(http.MethodGet, "/api/v1/system/info", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa login harus 401, dapat %d", rec.Code)
	}
}

func TestSetupFlowLogsInAndRunsOnce(t *testing.T) {
	h := newHarness(t, &fakeAgent{results: map[string]any{proto.OpSystemInfo: proto.SystemInfo{Hostname: "sapo-lab"}}})
	var st map[string]any
	_ = json.Unmarshal(h.do(http.MethodGet, "/api/v1/auth/status", nil).Body.Bytes(), &st)
	if st["setup_required"] != true {
		t.Fatalf("status awal %v", st)
	}
	h.runSetup()

	if rec := h.do(http.MethodGet, "/api/v1/system/info", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sapo-lab") {
		t.Fatalf("setelah setup harus sudah login: %d %s", rec.Code, rec.Body)
	}
	if h.cookie == nil || !h.cookie.HttpOnly || h.cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie sesi tidak aman: %+v", h.cookie)
	}
	// Setup kedua (misalnya penyerang di LAN) harus ditolak.
	h.cookie = nil
	if rec := h.do(http.MethodPost, "/api/v1/setup/totp", map[string]string{}); rec.Code != http.StatusConflict {
		t.Fatalf("setup ulang harus 409, dapat %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/setup", setupRequest{Username: "penyusup", Password: "xxxxxxxxxxxx", PasswordConfirm: "xxxxxxxxxxxx"}); rec.Code != http.StatusConflict {
		t.Fatalf("setup ulang harus 409, dapat %d", rec.Code)
	}
	data, _ := os.ReadFile(filepath.Join(h.data, "users.json"))
	if strings.Contains(string(data), "lumbung padi") {
		t.Fatal("password tersimpan sebagai teks biasa")
	}
	if !strings.Contains(string(data), `"username": "arthur"`) {
		t.Fatal("username harus disimpan huruf kecil")
	}
	logData, _ := os.ReadFile(filepath.Join(h.data, "audit.log"))
	if !strings.Contains(string(logData), `"action":"setup.complete"`) {
		t.Fatal("setup tidak tercatat di log audit")
	}
}

func TestSetupValidation(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	cases := []struct {
		req  setupRequest
		code string
	}{
		{setupRequest{Username: "ab", Password: "panjang sekali", PasswordConfirm: "panjang sekali"}, "username_invalid"},
		{setupRequest{Username: "root", Password: "panjang sekali", PasswordConfirm: "panjang sekali"}, "username_reserved"},
		{setupRequest{Username: "arthur", Password: "pendek", PasswordConfirm: "pendek"}, "password_too_short"},
		{setupRequest{Username: "arthur", Password: "panjang sekali", PasswordConfirm: "beda sekali!!"}, "password_mismatch"},
		{setupRequest{Username: "arthur", Password: "panjang sekali", PasswordConfirm: "panjang sekali", TOTPCode: "123456"}, "totp_expired"},
	}
	for _, c := range cases {
		if rec := h.do(http.MethodPost, "/api/v1/setup", c.req); errCode(rec) != c.code {
			t.Errorf("%+v: harus %s, dapat %d %s", c.req, c.code, rec.Code, rec.Body)
		}
	}
	var tr setupTOTPResponse
	_ = json.Unmarshal(h.do(http.MethodPost, "/api/v1/setup/totp", map[string]string{}).Body.Bytes(), &tr)
	// Klien lain membuat pendaftaran baru: pendaftaran pertama tetap berlaku.
	h.do(http.MethodPost, "/api/v1/setup/totp", map[string]string{}, fromIP("192.168.1.66"))
	if rec := h.do(http.MethodPost, "/api/v1/setup", setupRequest{Username: "arthur", Password: "panjang sekali", PasswordConfirm: "panjang sekali", TOTPCode: "000000", TOTPToken: tr.Token}); errCode(rec) != "totp_invalid" {
		t.Fatalf("kode 2FA salah harus ditolak: %s", rec.Body)
	}
}

func TestLoginTwoStepLogoutAndReplay(t *testing.T) {
	h := newHarness(t, &fakeAgent{results: map[string]any{proto.OpSystemInfo: proto.SystemInfo{}}})
	secret := h.runSetup()
	h.do(http.MethodPost, "/api/v1/auth/logout", nil)
	if rec := h.do(http.MethodGet, "/api/v1/system/info", nil); rec.Code != http.StatusUnauthorized {
		t.Fatal("setelah logout harus 401")
	}

	if rec := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit"}); errCode(rec) != "totp_required" {
		t.Fatalf("harus minta kode 2FA: %s", rec.Body)
	}
	h.now = h.now.Add(2 * time.Minute)
	code, _ := auth.TOTPCodeAt(secret, h.now)
	if rec := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "ARTHUR", Password: "lumbung padi di bukit", TOTPCode: code}); rec.Code != http.StatusOK {
		t.Fatalf("login gagal: %d %s", rec.Code, rec.Body)
	}
	h.do(http.MethodPost, "/api/v1/auth/logout", nil)
	// Kode yang sama tidak boleh dipakai ulang.
	if rec := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit", TOTPCode: code}); errCode(rec) != "totp_invalid" {
		t.Fatalf("kode 2FA bekas harus ditolak: %s", rec.Body)
	}
}

func TestLoginLockout(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	secret := h.runSetup()
	h.cookie = nil
	for i := 0; i < h.s.cfg.Auth.LoginMaxAttempts; i++ {
		h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "salah"})
	}
	rec := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("IP harus terkunci, dapat %d %s", rec.Code, rec.Body)
	}
	// Penyerang dari IP lain mengunci akun arthur...
	for i := 0; i < h.s.cfg.Auth.LoginMaxAttempts+1; i++ {
		h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "salah"}, fromIP(fmt.Sprintf("10.9.0.%d", i+1)))
	}
	// ...tetapi admin dengan password + 2FA yang benar tetap bisa masuk dari IP-nya sendiri.
	h.now = h.now.Add(time.Minute)
	code, _ := auth.TOTPCodeAt(secret, h.now)
	rec = h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit", TOTPCode: code}, fromIP("192.168.1.77"))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin dengan 2FA harus tetap bisa masuk walau akunnya diserang: %d %s", rec.Code, rec.Body)
	}
	// User yang tidak ada mendapat pesan yang sama dengan password salah.
	rec = h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "tidakada", Password: "apa saja"}, fromIP("10.0.0.9"))
	if errCode(rec) != "bad_credentials" {
		t.Fatalf("username tidak ada: %s", rec.Body)
	}
}

func TestCSRFAndOrigin(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	if rec := h.do(http.MethodPost, "/api/v1/setup/totp", map[string]string{}, noCSRF()); rec.Code != http.StatusForbidden {
		t.Fatalf("tanpa header CSRF harus 403, dapat %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/setup/totp", map[string]string{}, origin("https://situs-jahat.example")); rec.Code != http.StatusForbidden {
		t.Fatalf("origin asing harus 403, dapat %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/setup/totp", map[string]string{}, origin("http://example.com")); rec.Code != http.StatusOK {
		t.Fatalf("origin sama harus diterima, dapat %d %s", rec.Code, rec.Body)
	}
}

func TestNetworkPolicy(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	for _, ip := range []string{"127.0.0.1", "192.168.1.5", "10.1.2.3", "::1"} {
		if rec := h.do(http.MethodGet, "/api/v1/auth/status", nil, fromIP(ip)); rec.Code != http.StatusOK {
			t.Errorf("%s harus diizinkan, dapat %d", ip, rec.Code)
		}
	}
	for _, ip := range []string{"8.8.8.8", "203.0.113.7", "2001:db8::1"} {
		if rec := h.do(http.MethodGet, "/api/v1/auth/status", nil, fromIP(ip)); rec.Code != http.StatusForbidden {
			t.Errorf("%s (internet) harus ditolak, dapat %d", ip, rec.Code)
		}
	}
}

func TestAgentDownGives503(t *testing.T) {
	h := newHarness(t, &fakeAgent{err: errors.New("dial unix: no such file")})
	h.runSetup()
	rec := h.do(http.MethodGet, "/api/v1/system/info", nil)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "dial unix") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

func TestSecurityHeadersAndStatic(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	rec := h.do(http.MethodGet, "/storage/pools", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Sapo") {
		t.Fatalf("fallback SPA gagal: %d", rec.Code)
	}
	for _, hd := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options", "Referrer-Policy"} {
		if rec.Header().Get(hd) == "" {
			t.Errorf("header %s tidak ada", hd)
		}
	}
	if rec := h.do(http.MethodGet, "/../../../../etc/passwd", nil); strings.Contains(rec.Body.String(), "root:") {
		t.Fatal("path traversal berhasil")
	}
	if rec := h.do(http.MethodGet, "/api/v1/tidak-ada", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestUIConfigBrandingAndPolicy(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	body := h.do(http.MethodGet, "/api/v1/ui/config", nil).Body.String()
	for _, want := range []string{`"full_name":"Sapo by Mehuli"`, `"default_mode":"desktop"`, `"password_min_length":10`} {
		if !strings.Contains(body, want) {
			t.Errorf("tidak ada %s di %s", want, body)
		}
	}
	if strings.Contains(body, "signing_key") {
		t.Fatal("bagian updates tidak boleh dikirim ke panel")
	}
	bad := filepath.Join(t.TempDir(), "branding.yaml")
	_ = os.WriteFile(bad, []byte("theme:\n  light:\n    primary: \"red; } body { display:none\"\n"), 0o644)
	if _, err := loadBranding(bad); err == nil {
		t.Fatal("nilai CSS berbahaya harus ditolak")
	}
}

func TestWizards(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	rec := h.do(http.MethodGet, "/api/v1/wizards/setup", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"totp-enroll"`) {
		t.Fatalf("wizard setup: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/wizards/..%2Fsapo", nil); rec.Code == http.StatusOK {
		t.Fatal("id wizard aneh harus ditolak")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "rusak.yaml"), []byte("id: rusak\nsteps:\n  - id: a\n    fields:\n      - {key: x, type: shell}\n"), 0o644)
	if _, err := LoadWizard(dir, "rusak"); err == nil {
		t.Fatal("jenis isian tak dikenal harus ditolak")
	}
}

func TestDNSRebindingHostRejected(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	setHost := func(host string) reqOpt { return func(r *http.Request) { r.Host = host } }
	for _, host := range []string{"evil.example:5000", "evil.example", "nas.attacker.test"} {
		rec := h.do(http.MethodPost, "/api/v1/setup/totp", map[string]string{}, setHost(host), origin("http://"+host))
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("host %s harus ditolak, dapat %d", host, rec.Code)
		}
	}
	for _, host := range []string{"192.168.1.10:5000", "[fe80::1]:5000", "localhost:5173", "example.com"} {
		if rec := h.do(http.MethodGet, "/api/v1/auth/status", nil, setHost(host)); rec.Code != http.StatusOK {
			t.Errorf("host %s harus diizinkan, dapat %d", host, rec.Code)
		}
	}
}

func TestParallelLoginsCannotBypassLimit(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	h.runSetup()
	h.cookie = nil
	var wg sync.WaitGroup
	var mu sync.Mutex
	checked := 0
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"arthur","password":"salah"}`))
			r.RemoteAddr = "192.168.1.200:1234"
			r.Header.Set(CSRFHeader, "1")
			rec := httptest.NewRecorder()
			h.s.ServeHTTP(rec, r)
			if rec.Code == http.StatusUnauthorized {
				mu.Lock()
				checked++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if checked > h.s.cfg.Auth.LoginMaxAttempts {
		t.Fatalf("%d password diperiksa, batasnya %d", checked, h.s.cfg.Auth.LoginMaxAttempts)
	}
}

func TestInvalidUsernameNotLoggedRaw(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	h.runSetup()
	h.cookie = nil
	long := strings.Repeat("A", 5000)
	h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: long, Password: "x"})
	data, _ := os.ReadFile(filepath.Join(h.data, "audit.log"))
	if strings.Contains(string(data), strings.Repeat("a", 100)) {
		t.Fatal("username tidak valid tercatat apa adanya di log audit")
	}
}

func TestNetworkEdgeCases(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	if rec := h.do(http.MethodGet, "/api/v1/auth/status", nil, func(r *http.Request) { r.RemoteAddr = "[fe80::1%eth0]:4000" }); rec.Code != http.StatusOK {
		t.Errorf("link-local dengan zone harus diizinkan, dapat %d", rec.Code)
	}
	if rec := h.do(http.MethodGet, "/api/v1/auth/status", nil, fromIP("172.17.0.5")); rec.Code != http.StatusForbidden {
		t.Errorf("jaringan Docker harus ditolak, dapat %d", rec.Code)
	}
	if rec := h.do(http.MethodGet, "/api/v1/auth/status", nil, fromIP("100.70.1.1")); rec.Code != http.StatusForbidden {
		t.Errorf("CGNAT tidak boleh dibuka secara default, dapat %d", rec.Code)
	}
}

func TestDotfilesNotServed(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	_ = os.WriteFile(filepath.Join(h.s.cfg.API.WebDir, ".env"), []byte("RAHASIA=1"), 0o644)
	if rec := h.do(http.MethodGet, "/.env", nil); strings.Contains(rec.Body.String(), "RAHASIA") {
		t.Fatal("dotfile tersaji")
	}
}

func TestTrailingJSONRejected(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"a","password":"b"} {"x":1}`))
	r.RemoteAddr = "192.168.1.20:1"
	r.Header.Set(CSRFHeader, "1")
	rec := httptest.NewRecorder()
	h.s.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("data tambahan setelah JSON harus ditolak, dapat %d", rec.Code)
	}
}

func TestLockedAccountGivesNoPasswordOracle(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	secret := h.runSetup()
	h.cookie = nil
	// Kunci akun dari banyak jaringan berbeda.
	for i := 0; i < h.s.cfg.Auth.LoginMaxAttempts+1; i++ {
		h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "salah"}, fromIP(fmt.Sprintf("10.8.0.%d", i+1)))
	}
	// Password benar maupun salah tanpa kode: jawaban sama.
	right := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit"}, fromIP("10.8.1.1"))
	wrong := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "tebakan"}, fromIP("10.8.1.2"))
	if errCode(right) != errCode(wrong) || right.Code != wrong.Code {
		t.Fatalf("jawaban berbeda saat terkunci: %d %s vs %d %s", right.Code, right.Body, wrong.Code, wrong.Body)
	}
	// Password benar + kode salah vs password salah + kode salah: jawaban sama.
	a := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit", TOTPCode: "000000"}, fromIP("10.8.1.3"))
	b := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "tebakan", TOTPCode: "000000"}, fromIP("10.8.1.4"))
	if errCode(a) != errCode(b) || a.Code != b.Code {
		t.Fatalf("oracle password saat terkunci: %d %s vs %d %s", a.Code, a.Body, b.Code, b.Body)
	}
	// Admin sungguhan tetap bisa masuk dengan password + kode dalam satu permintaan.
	h.now = h.now.Add(time.Minute)
	code, _ := auth.TOTPCodeAt(secret, h.now)
	if rec := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit", TOTPCode: code}, fromIP("192.168.1.9")); rec.Code != http.StatusOK {
		t.Fatalf("admin harus tetap bisa masuk: %d %s", rec.Code, rec.Body)
	}
}

func TestIPv6RotationWithinPrefixCountsTogether(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	h.runSetup()
	h.cookie = nil
	locked := false
	for i := 1; i <= 20; i++ {
		rec := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "tidakada", Password: "x"}, fromIP(fmt.Sprintf("fd00::%x", i)))
		if rec.Code == http.StatusTooManyRequests {
			locked = true
			break
		}
	}
	if !locked {
		t.Fatal("alamat IPv6 berbeda dalam satu /64 harus dihitung bersama")
	}
}

func TestTOTPTyposDoNotLockAdmin(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	secret := h.runSetup()
	h.cookie = nil
	for i := 0; i < 2; i++ { // dua login normal sebelumnya
		h.now = h.now.Add(time.Minute)
		code, _ := auth.TOTPCodeAt(secret, h.now)
		h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit"})
		if rec := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit", TOTPCode: code}); rec.Code != http.StatusOK {
			t.Fatalf("login normal gagal: %s", rec.Body)
		}
		h.do(http.MethodPost, "/api/v1/auth/logout", nil)
	}
	h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit"})
	for i := 0; i < 6; i++ { // salah ketik kode berkali-kali
		h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit", TOTPCode: "111111"})
	}
	h.now = h.now.Add(time.Minute)
	code, _ := auth.TOTPCodeAt(secret, h.now)
	if rec := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit", TOTPCode: code}); rec.Code != http.StatusOK {
		t.Fatalf("salah ketik kode tidak boleh mengunci admin: %d %s", rec.Code, rec.Body)
	}
}

func TestTOTPGuessingCapped(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	secret := h.runSetup()
	h.cookie = nil
	for i := 0; i < h.s.cfg.Auth.TOTPMaxFailures; i++ {
		h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit", TOTPCode: fmt.Sprintf("%06d", i)}, fromIP(fmt.Sprintf("10.7.0.%d", i+1)))
	}
	h.now = h.now.Add(time.Minute)
	code, _ := auth.TOTPCodeAt(secret, h.now)
	if rec := h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: "lumbung padi di bukit", TOTPCode: code}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("setelah batas kode salah, akun harus ditahan: %d %s", rec.Code, rec.Body)
	}
}

func TestSetupSlotsPerClient(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	for i := 0; i < 50; i++ {
		h.do(http.MethodPost, "/api/v1/setup/totp", map[string]string{}, fromIP("192.168.1.66"))
	}
	if rec := h.do(http.MethodPost, "/api/v1/setup/totp", map[string]string{}, fromIP("192.168.1.10")); rec.Code != http.StatusOK {
		t.Fatalf("satu klien tidak boleh memenuhi semua slot setup: %d", rec.Code)
	}
}

func TestTrailingBracesRejected(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	for _, body := range []string{`{"username":"a","password":"b"}}`, `{"username":"a","password":"b"}]`} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
		r.RemoteAddr = "192.168.1.20:1"
		r.Header.Set(CSRFHeader, "1")
		rec := httptest.NewRecorder()
		h.s.ServeHTTP(rec, r)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s harus ditolak, dapat %d", body, rec.Code)
		}
	}
}

func TestStorageEndpoints(t *testing.T) {
	agent := &fakeAgent{results: map[string]any{
		proto.OpStorageDisks: []proto.Disk{{Name: "sda", Path: "/dev/sda", SizeBytes: 1 << 40, Partitions: []proto.Partition{}}},
		proto.OpStorageSMART: proto.SMART{Device: "sda", Status: proto.SMARTPassed, Attributes: []proto.SMARTAttr{}},
	}}
	h := newHarness(t, agent)
	for _, p := range []string{"/api/v1/storage/disks", "/api/v1/storage/disks/sda/smart"} {
		if rec := h.do(http.MethodGet, p, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s tanpa login harus 401, dapat %d", p, rec.Code)
		}
	}
	h.runSetup()

	rec := h.do(http.MethodGet, "/api/v1/storage/disks", nil)
	var disks []proto.Disk
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &disks) != nil || len(disks) != 1 || disks[0].Name != "sda" {
		t.Fatalf("disks %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("data disk tidak boleh di-cache")
	}

	rec = h.do(http.MethodGet, "/api/v1/storage/disks/sda/smart", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("smart %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.params[proto.OpStorageSMART].(proto.SMARTParams); p.Device != "sda" {
		t.Fatalf("parameter ke sapod = %#v", agent.params[proto.OpStorageSMART])
	}

	// Kode error sapod dipetakan ke status HTTP yang sesuai.
	for code, status := range map[string]int{
		proto.ErrNotFound:      http.StatusNotFound,
		proto.ErrInvalidParams: http.StatusBadRequest,
		proto.ErrUnavailable:   http.StatusServiceUnavailable,
		proto.ErrBusy:          http.StatusServiceUnavailable,
	} {
		agent.errs = map[string]error{proto.OpStorageSMART: &proto.Error{Code: code, Message: "x"}}
		rec = h.do(http.MethodGet, "/api/v1/storage/disks/sdz/smart", nil)
		if rec.Code != status || errCode(rec) != code {
			t.Errorf("%s: dapat %d %s, harus %d", code, rec.Code, errCode(rec), status)
		}
	}
}

func viaTLS() reqOpt { return func(r *http.Request) { r.TLS = &tls.ConnectionState{} } }

func newHTTPSHarness(t *testing.T, agent AgentCaller) *harness {
	h := newHarness(t, agent)
	h.s.https = &HTTPSInfo{Port: "5001", Fingerprint: func() string { return "AB:CD" }}
	return h
}

func TestHTTPSRequiredBlocksAPIOverHTTP(t *testing.T) {
	h := newHTTPSHarness(t, &fakeAgent{results: map[string]any{proto.OpSystemInfo: proto.SystemInfo{}}})

	// Lewat HTTP hanya panel dan data tanpa rahasia untuk halaman petunjuk.
	if rec := h.do(http.MethodGet, "/", nil); rec.Code != http.StatusOK {
		t.Fatalf("panel lewat HTTP: %d", rec.Code)
	}
	rec := h.do(http.MethodGet, "/api/v1/ui/config", nil)
	var cfg struct {
		HTTPS struct {
			Port        string `json:"port"`
			Fingerprint string `json:"fingerprint_sha256"`
		} `json:"https"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &cfg) != nil || cfg.HTTPS.Port != "5001" || cfg.HTTPS.Fingerprint != "AB:CD" {
		t.Fatalf("ui/config lewat HTTP: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/health", nil); rec.Code != http.StatusOK {
		t.Fatalf("health lewat HTTP: %d", rec.Code)
	}

	// Apa pun yang membawa password, sesi, atau data sistem ditolak.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/auth/status"},
		{http.MethodPost, "/api/v1/auth/login"},
		{http.MethodPost, "/api/v1/setup/totp"},
		{http.MethodPost, "/api/v1/setup"},
		{http.MethodGet, "/api/v1/system/info"},
		{http.MethodGet, "/api/v1/wizards/setup"},
		{http.MethodGet, "/api/v1/tidak-ada"},
	} {
		rec := h.do(c.method, c.path, map[string]string{"username": "arthur", "password": "x"})
		if rec.Code != http.StatusForbidden || errCode(rec) != "https_required" {
			t.Errorf("%s %s lewat HTTP: %d %s, harus 403 https_required", c.method, c.path, rec.Code, errCode(rec))
		}
	}
}

func TestHTTPSServesEverythingWithoutHSTS(t *testing.T) {
	h := newHTTPSHarness(t, &fakeAgent{results: map[string]any{proto.OpSystemInfo: proto.SystemInfo{Hostname: "sapo-lab"}}})
	rec := h.do(http.MethodGet, "/api/v1/auth/status", nil, viaTLS())
	if rec.Code != http.StatusOK {
		t.Fatalf("auth/status lewat HTTPS: %d", rec.Code)
	}
	// Sertifikat self-signed: HSTS akan mengunci admin bila sertifikat berganti.
	if v := rec.Header().Get("Strict-Transport-Security"); v != "" {
		t.Fatalf("HSTS tidak boleh dikirim dengan sertifikat self-signed: %q", v)
	}

	// Setup awal lewat HTTPS berjalan, dan cookie sesi bertanda Secure.
	tr := h.do(http.MethodPost, "/api/v1/setup/totp", map[string]string{"username": "arthur"}, viaTLS())
	var resp setupTOTPResponse
	_ = json.Unmarshal(tr.Body.Bytes(), &resp)
	code, _ := auth.TOTPCodeAt(resp.Secret, h.now)
	rec = h.do(http.MethodPost, "/api/v1/setup", setupRequest{
		Language: "id", Username: "arthur", Password: "lumbung padi di bukit", PasswordConfirm: "lumbung padi di bukit", TOTPCode: code, TOTPToken: resp.Token,
	}, viaTLS())
	if rec.Code != http.StatusOK {
		t.Fatalf("setup lewat HTTPS: %d %s", rec.Code, rec.Body)
	}
	var secure bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == SecureSessionCookie {
			secure = c.Secure && c.Path == "/" && c.Domain == ""
		}
	}
	if !secure {
		t.Fatal("cookie sesi lewat HTTPS harus __Host- (Secure, Path=/, tanpa Domain)")
	}
	// Cookie sesi tanpa awalan (misalnya ditanam lewat HTTP) diabaikan di HTTPS.
	planted := h.do(http.MethodGet, "/api/v1/auth/status", nil, viaTLS(), func(r *http.Request) {
		r.Header.Del("Cookie")
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: h.cookie.Value})
	})
	if strings.Contains(planted.Body.String(), `"authenticated":true`) {
		t.Fatal("cookie tanpa awalan __Host- tidak boleh diterima lewat HTTPS")
	}
	if rec := h.do(http.MethodGet, "/api/v1/system/info", nil, viaTLS()); rec.Code != http.StatusOK {
		t.Fatalf("system/info lewat HTTPS: %d", rec.Code)
	}
}

func TestHTTPOnlyModeUnchanged(t *testing.T) {
	// HTTPS dimatikan (misalnya make dev): HTTP melayani semuanya seperti dulu.
	h := newHarness(t, &fakeAgent{})
	if rec := h.do(http.MethodGet, "/api/v1/auth/status", nil); rec.Code != http.StatusOK {
		t.Fatalf("auth/status: %d", rec.Code)
	}
	var cfg map[string]any
	_ = json.Unmarshal(h.do(http.MethodGet, "/api/v1/ui/config", nil).Body.Bytes(), &cfg)
	if _, ok := cfg["https"]; ok {
		t.Fatal("ui/config tidak boleh memuat info HTTPS bila HTTPS mati")
	}
}

const testPassword = "lumbung padi di bukit"

func (h *harness) login(code string) *httptest.ResponseRecorder {
	h.t.Helper()
	h.cookie = nil
	return h.do(http.MethodPost, "/api/v1/auth/login", loginRequest{Username: "arthur", Password: testPassword, TOTPCode: code})
}

func recoveryLeft(t *testing.T, h *harness) (int, bool) {
	t.Helper()
	var st struct {
		User struct {
			Left *int `json:"recovery_codes_left"`
			Low  bool `json:"recovery_codes_low"`
		} `json:"user"`
	}
	_ = json.Unmarshal(h.do(http.MethodGet, "/api/v1/auth/status", nil).Body.Bytes(), &st)
	if st.User.Left == nil {
		t.Fatal("auth/status harus memuat recovery_codes_left")
	}
	return *st.User.Left, st.User.Low
}

func TestRecoveryCodesLoginOnce(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	h.runSetup()
	if len(h.recovery) != 10 {
		t.Fatalf("setup harus memberi 10 kode cadangan, dapat %d", len(h.recovery))
	}
	if left, low := recoveryLeft(t, h); left != 10 || low {
		t.Fatalf("sisa %d low %v", left, low)
	}

	// Kode cadangan diterima di kolom kode 2FA, dalam format apa pun.
	rec := h.login(strings.ToLower(strings.ReplaceAll(h.recovery[0], "-", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("login dengan kode cadangan: %d %s", rec.Code, rec.Body)
	}
	var lr struct {
		Left int `json:"recovery_codes_left"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &lr)
	if lr.Left != 9 {
		t.Fatalf("sisa setelah dipakai %d, harus 9", lr.Left)
	}
	// Sekali pakai.
	if rec := h.login(h.recovery[0]); rec.Code != http.StatusUnauthorized || errCode(rec) != "totp_invalid" {
		t.Fatalf("kode bekas: %d %s", rec.Code, errCode(rec))
	}
	// Pemakaian tercatat di log audit.
	b, _ := os.ReadFile(filepath.Join(h.data, "audit.log"))
	if !strings.Contains(string(b), `"method":"recovery_code"`) {
		t.Fatalf("log audit harus mencatat kode cadangan:\n%s", b)
	}
	// Peringatan saat sisa kode sedikit.
	for _, c := range h.recovery[1:7] {
		if rec := h.login(c); rec.Code != http.StatusOK {
			t.Fatalf("login %s: %d", c, rec.Code)
		}
	}
	if left, low := recoveryLeft(t, h); left != 3 || !low {
		t.Fatalf("sisa %d low %v, harus 3 dan low", left, low)
	}
}

func TestRecoveryCodesCountTowardTOTPLimit(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	h.runSetup()
	for i := 0; i < h.s.cfg.Auth.TOTPMaxFailures; i++ {
		h.login("AAAAA-AAAAA")
	}
	// Kode cadangan yang benar pun ditolak setelah batas tercapai.
	if rec := h.login(h.recovery[0]); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("setelah batas, harus 429, dapat %d", rec.Code)
	}
}

func TestRegenerateRecoveryCodesNeedsPassword(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	h.runSetup()
	if rec := h.do(http.MethodPost, "/api/v1/account/recovery-codes", map[string]string{"password": "salah sekali"}); rec.Code != http.StatusUnauthorized || errCode(rec) != "bad_password" {
		t.Fatalf("password salah: %d %s", rec.Code, errCode(rec))
	}
	rec := h.do(http.MethodPost, "/api/v1/account/recovery-codes", map[string]string{"password": testPassword})
	var out struct {
		Codes []string `json:"recovery_codes"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Codes) != 10 {
		t.Fatalf("buat ulang: %d %s", rec.Code, rec.Body)
	}
	// Kode lama tidak berlaku, kode baru berlaku.
	if rec := h.login(h.recovery[0]); rec.Code != http.StatusUnauthorized {
		t.Fatalf("kode lama harus ditolak, dapat %d", rec.Code)
	}
	if rec := h.login(out.Codes[0]); rec.Code != http.StatusOK {
		t.Fatalf("kode baru harus diterima, dapat %d", rec.Code)
	}
	// Tanpa login: ditolak.
	h.cookie = nil
	if rec := h.do(http.MethodPost, "/api/v1/account/recovery-codes", map[string]string{"password": testPassword}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa login harus 401, dapat %d", rec.Code)
	}
}

func TestReenrollTOTP(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	oldSecret := h.runSetup()

	if rec := h.do(http.MethodPost, "/api/v1/account/totp/begin", map[string]string{"password": "salah sekali"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("password salah: %d", rec.Code)
	}
	rec := h.do(http.MethodPost, "/api/v1/account/totp/begin", map[string]string{"password": testPassword})
	var tr setupTOTPResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tr) != nil || tr.Secret == "" || tr.Secret == oldSecret {
		t.Fatalf("begin: %d %s", rec.Code, rec.Body)
	}
	// Token palsu atau kode salah: rahasia lama tetap berlaku.
	if rec := h.do(http.MethodPost, "/api/v1/account/totp/confirm", map[string]string{"token": "palsu", "code": "000000"}); errCode(rec) != "totp_expired" {
		t.Fatalf("token palsu: %s", errCode(rec))
	}
	if rec := h.do(http.MethodPost, "/api/v1/account/totp/confirm", map[string]string{"token": tr.Token, "code": "000000"}); errCode(rec) != "totp_invalid" {
		t.Fatalf("kode salah: %s", errCode(rec))
	}
	h.now = h.now.Add(31 * time.Second)
	code, _ := auth.TOTPCodeAt(tr.Secret, h.now)
	if rec := h.do(http.MethodPost, "/api/v1/account/totp/confirm", map[string]string{"token": tr.Token, "code": code}); rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}
	// Kode yang dipakai untuk konfirmasi tidak bisa dipakai ulang untuk login.
	if rec := h.login(code); rec.Code != http.StatusUnauthorized {
		t.Fatalf("kode konfirmasi dipakai ulang: %d", rec.Code)
	}
	h.now = h.now.Add(31 * time.Second)
	oldCode, _ := auth.TOTPCodeAt(oldSecret, h.now)
	if rec := h.login(oldCode); rec.Code != http.StatusUnauthorized {
		t.Fatalf("rahasia lama harus tidak berlaku, dapat %d", rec.Code)
	}
	h.now = h.now.Add(31 * time.Second)
	newCode, _ := auth.TOTPCodeAt(tr.Secret, h.now)
	if rec := h.login(newCode); rec.Code != http.StatusOK {
		t.Fatalf("rahasia baru harus berlaku, dapat %d %s", rec.Code, rec.Body)
	}
	// Token sudah terpakai.
	if rec := h.do(http.MethodPost, "/api/v1/account/totp/confirm", map[string]string{"token": tr.Token, "code": newCode}); errCode(rec) != "totp_expired" {
		t.Fatalf("token bekas: %s", errCode(rec))
	}
}
