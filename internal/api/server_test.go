package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func newTestServer(t *testing.T, agent AgentCaller) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.API.WebDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg.API.WebDir, "index.html"), []byte("<!doctype html><title>Sapo</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return New(cfg, agent, nil)
}

func do(s *Server, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestSystemInfoIsProxied(t *testing.T) {
	s := newTestServer(t, &fakeAgent{results: map[string]any{
		proto.OpSystemInfo: proto.SystemInfo{Hostname: "sapo-lab"},
	}})
	rec := do(s, http.MethodGet, "/api/v1/system/info")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sapo-lab") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

func TestAgentDownGives503(t *testing.T) {
	s := newTestServer(t, &fakeAgent{err: errors.New("dial unix: no such file")})
	rec := do(s, http.MethodGet, "/api/v1/system/info")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "dial unix") {
		t.Fatal("detail error internal bocor ke klien")
	}
}

func TestSecurityHeaders(t *testing.T) {
	s := newTestServer(t, &fakeAgent{})
	rec := do(s, http.MethodGet, "/")
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options", "Referrer-Policy"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("header %s tidak ada", h)
		}
	}
}

func TestSPAFallbackAndTraversal(t *testing.T) {
	s := newTestServer(t, &fakeAgent{})
	if rec := do(s, http.MethodGet, "/storage/pools"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Sapo") {
		t.Fatalf("fallback SPA gagal: %d", rec.Code)
	}
	rec := do(s, http.MethodGet, "/../../../../etc/passwd")
	if strings.Contains(rec.Body.String(), "root:") {
		t.Fatal("path traversal berhasil")
	}
}

func TestUnknownAPIRouteIs404(t *testing.T) {
	s := newTestServer(t, &fakeAgent{})
	if rec := do(s, http.MethodGet, "/api/v1/tidak-ada"); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestUIConfigComesFromConfig(t *testing.T) {
	s := newTestServer(t, &fakeAgent{})
	rec := do(s, http.MethodGet, "/api/v1/ui/config")
	if !strings.Contains(rec.Body.String(), `"default_mode":"desktop"`) {
		t.Fatalf("body %s", rec.Body)
	}
}

func TestEnsureLoopback(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:5000", "localhost:5000", "[::1]:5000"} {
		if err := EnsureLoopback(ok); err != nil {
			t.Errorf("%s seharusnya diizinkan: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:5000", ":5000", "192.168.1.10:5000", "[::]:5000"} {
		if err := EnsureLoopback(bad); err == nil {
			t.Errorf("%s seharusnya ditolak", bad)
		}
	}
}

func TestBrandingIsServedAndValidated(t *testing.T) {
	s := newTestServer(t, &fakeAgent{})
	s.cfg.API.BrandingFile = "../../branding.yaml"
	rec := do(s, http.MethodGet, "/api/v1/ui/config")
	body := rec.Body.String()
	if !strings.Contains(body, `"full_name":"Sapo by Mehuli"`) {
		t.Fatalf("branding tidak terkirim: %s", body)
	}
	if strings.Contains(body, "signing_key") {
		t.Fatal("bagian updates tidak boleh dikirim ke panel")
	}

	bad := filepath.Join(t.TempDir(), "branding.yaml")
	if err := os.WriteFile(bad, []byte("theme:\n  light:\n    primary: \"red; } body { display:none\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBranding(bad); err == nil {
		t.Fatal("nilai CSS berbahaya harus ditolak")
	}
}
