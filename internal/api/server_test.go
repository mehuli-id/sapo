package api

import (
	"bytes"
	"context"
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
}

func (f *fakeAgent) Call(_ context.Context, op string, _ any, out any) error {
	if f.err != nil {
		return f.err
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
	al, err := audit.Open(h.data)
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
		if c.Name == SessionCookie {
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
