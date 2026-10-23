// Package api berisi sapo-api: server HTTP untuk panel dan aplikasi.
//
// sapo-api berjalan tanpa hak root dan tidak pernah menjalankan perintah
// sistem. Semua perubahan sistem diteruskan ke sapod sebagai operasi bernama.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/audit"
	"github.com/mehuli-id/sapo/internal/auth"
	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/proto"
)

// AgentCaller adalah kemampuan memanggil operasi sapod (diimplementasikan
// oleh agent.Client; di test diganti tiruan).
type AgentCaller interface {
	Call(ctx context.Context, op string, params any, out any) error
}

// Options adalah dependensi server.
type Options struct {
	Config config.Config
	Agent  AgentCaller
	Store  *auth.Store
	Audit  *audit.Log
	Log    *slog.Logger
	Now    func() time.Time
}

// Server adalah handler HTTP sapo-api.
type Server struct {
	cfg      config.Config
	agent    AgentCaller
	store    *auth.Store
	audit    *audit.Log
	log      *slog.Logger
	now      func() time.Time
	mux      *http.ServeMux
	networks []netip.Prefix
	denied   []netip.Prefix
	hosts    map[string]bool
	sessions *auth.Sessions
	limiter  *auth.Limiter
	// totpLimiter menghitung kode 2FA salah per akun.
	totpLimiter *auth.Limiter
	userRe      *regexp.Regexp
	argon       auth.Argon2Params
	dummy       string
	setup       pendingTOTP

	hashSlots chan struct{}

	healthMu      sync.Mutex
	healthAt      time.Time
	healthOK      bool
	healthRunning bool
}

// SessionCookie adalah nama cookie sesi panel.
const SessionCookie = "sapo_session"

// CSRFHeader wajib ada di setiap permintaan yang mengubah data. Browser tidak
// bisa mengirim header khusus lintas situs tanpa izin CORS (yang tidak pernah
// diberikan sapo-api), sehingga situs lain tidak bisa memalsukan permintaan.
const CSRFHeader = "X-Sapo-Request"

// New menyusun server beserta rutenya.
func New(o Options) (*Server, error) {
	cfg := o.Config
	nets, err := cfg.Networks()
	if err != nil {
		return nil, err
	}
	denied, err := cfg.Denied()
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	argon := auth.Argon2Params{MemoryKiB: cfg.Auth.Argon2.MemoryKiB, Iterations: cfg.Auth.Argon2.Iterations, Parallelism: cfg.Auth.Argon2.Parallelism}
	dummy, err := auth.HashPassword("sapo-dummy-password", argon)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, agent: o.Agent, store: o.Store, audit: o.Audit, log: o.Log, now: o.Now,
		mux:      http.NewServeMux(),
		networks: nets,
		denied:   denied,
		hosts:    allowedHosts(cfg.API.Hostnames),
		sessions: auth.NewSessions(time.Duration(cfg.Auth.SessionIdleMinutes)*time.Minute, time.Duration(cfg.Auth.SessionMaxHours)*time.Hour),
		limiter: auth.NewLimiter(cfg.Auth.LoginMaxAttempts,
			time.Duration(cfg.Auth.LoginWindowMinutes)*time.Minute, time.Duration(cfg.Auth.LockoutMinutes)*time.Minute),
		totpLimiter: auth.NewLimiter(cfg.Auth.TOTPMaxFailures,
			time.Duration(cfg.Auth.TOTPWindowMinutes)*time.Minute, time.Duration(cfg.Auth.TOTPWindowMinutes)*time.Minute),
		userRe: regexp.MustCompile(cfg.Auth.UsernamePattern),
		argon:  argon,
		dummy:  dummy,
		setup:  pendingTOTP{entries: map[string]pendingEntry{}},

		hashSlots: make(chan struct{}, cfg.Auth.MaxConcurrentHashes),
	}

	// Penghitung akun yang ada tidak boleh terhapus oleh banjir entri lain.
	protect := func(key string) bool {
		name, ok := strings.CutPrefix(key, "user:")
		if !ok {
			name, ok = strings.CutPrefix(key, "totp:")
		}
		if !ok {
			return false
		}
		_, exists := s.store.Get(name)
		return exists
	}
	s.limiter.Protected = protect
	s.totpLimiter.Protected = protect

	// Publik: dibutuhkan sebelum login.
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/ui/config", s.handleUIConfig)
	s.mux.HandleFunc("GET /api/v1/auth/status", s.handleAuthStatus)
	s.mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/v1/wizards/{id}", s.handleWizard)
	s.mux.HandleFunc("POST /api/v1/setup/totp", s.handleSetupTOTP)
	s.mux.HandleFunc("POST /api/v1/setup", s.handleSetup)

	// Butuh login.
	s.mux.Handle("GET /api/v1/system/info", s.requireAuth(s.proxyRead(proto.OpSystemInfo)))
	s.mux.Handle("GET /api/v1/features", s.requireAuth(s.proxyRead(proto.OpFeaturesList)))
	s.mux.Handle("GET /api/v1/storage/disks", s.requireAuth(s.proxyRead(proto.OpStorageDisks)))
	s.mux.Handle("GET /api/v1/storage/disks/{name}/smart", s.requireAuth(http.HandlerFunc(s.handleDiskSMART)))

	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "endpoint tidak ada")
	})
	s.mux.Handle("/", s.staticHandler())
	return s, nil
}

// ServeHTTP menerapkan kebijakan jaringan, perlindungan CSRF, header
// keamanan, dan log ke setiap permintaan.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	if r.TLS != nil {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	switch {
	case !s.allowedClient(r):
		writeError(rec, http.StatusForbidden, "network_not_allowed", "akses dari jaringan ini tidak diizinkan")
	case !s.allowedHost(r.Host):
		// Melindungi dari DNS rebinding: situs lain yang mengarahkan domainnya
		// ke IP NAS tetap ditolak karena header Host-nya bukan milik NAS.
		writeError(rec, http.StatusMisdirectedRequest, "host_not_allowed", "nama host tidak dikenal")
	case isMutating(r.Method) && !s.csrfOK(r):
		writeError(rec, http.StatusForbidden, "csrf", "permintaan ditolak")
	default:
		s.mux.ServeHTTP(rec, r)
	}
	if s.log != nil && strings.HasPrefix(r.URL.Path, "/api/") {
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ip", clientIP(r), "durasi", s.now().Sub(start))
	}
}

func isMutating(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// csrfOK: header khusus wajib ada, dan bila browser mengirim Origin, host-nya
// harus sama dengan host panel.
func (s *Server) csrfOK(r *http.Request) bool {
	if r.Header.Get(CSRFHeader) != "1" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}

// clientIP memakai alamat koneksi langsung. Header X-Forwarded-For sengaja
// tidak dipercaya sampai reverse proxy bawaan Sapo tersedia.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clientNet adalah kunci batas percobaan: alamat IPv4 penuh, atau prefix /64
// untuk IPv6 (satu perangkat bisa memakai banyak alamat dalam satu /64).
func clientNet(r *http.Request) string {
	addr, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return clientIP(r)
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

func (s *Server) allowedClient(r *http.Request) bool {
	addr, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	for _, p := range s.denied {
		if p.Contains(addr) {
			return false
		}
	}
	for _, p := range s.networks {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// allowedHosts menyusun nama host yang sah untuk panel: localhost, nama host
// mesin, <nama host>.local, dan daftar tambahan dari konfigurasi.
func allowedHosts(extra []string) map[string]bool {
	out := map[string]bool{"localhost": true}
	if h, err := os.Hostname(); err == nil && h != "" {
		h = strings.ToLower(h)
		out[h] = true
		out[h+".local"] = true
	}
	for _, h := range extra {
		out[strings.ToLower(strings.TrimSuffix(h, "."))] = true
	}
	return out
}

// allowedHost menerima alamat IP apa pun (akses langsung lewat IP tidak bisa
// dipakai untuk DNS rebinding) dan nama host yang terdaftar.
func (s *Server) allowedHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return s.hosts[host]
}

// handleHealth publik, jadi hasilnya di-cache beberapa detik agar tidak bisa
// dipakai untuk membanjiri sapod.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.healthMu.Lock()
	refresh := s.now().Sub(s.healthAt) > 5*time.Second && !s.healthRunning
	if refresh {
		s.healthRunning = true
	}
	agentOK := s.healthOK
	s.healthMu.Unlock()
	// Hanya satu pemeriksaan berjalan pada satu waktu, dengan batas waktu
	// pendek; permintaan lain memakai hasil terakhir.
	if refresh {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		ok := s.agent.Call(ctx, proto.OpSystemInfo, nil, nil) == nil
		cancel()
		s.healthMu.Lock()
		s.healthOK, s.healthAt, s.healthRunning = ok, s.now(), false
		s.healthMu.Unlock()
		agentOK = ok
	}
	status := http.StatusOK
	if !agentOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"status": map[bool]string{true: "ok", false: "degraded"}[agentOK], "agent": agentOK})
}

// handleUIConfig memberi panel nilai default, kebijakan isian, dan branding
// dari konfigurasi (bukan dari kode panel).
func (s *Server) handleUIConfig(w http.ResponseWriter, _ *http.Request) {
	lang := s.cfg.API.DefaultLanguage
	if l := s.store.Language(); l != "" {
		lang = l
	}
	resp := map[string]any{
		"default_mode":     s.cfg.UI.DefaultMode,
		"default_language": lang,
		"auth_policy": map[string]any{
			"password_min_length": s.cfg.Auth.PasswordMinLength,
			"username_pattern":    s.cfg.Auth.UsernamePattern,
		},
	}
	if b, err := loadBranding(s.cfg.API.BrandingFile); err == nil {
		resp["branding"] = b
	} else if s.log != nil {
		s.log.Warn("branding tidak terbaca", "file", s.cfg.API.BrandingFile, "err", err)
	}
	writeJSON(w, http.StatusOK, resp)
}

// proxyRead meneruskan operasi baca-saja ke sapod tanpa parameter.
func (s *Server) proxyRead(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var out json.RawMessage
		if err := s.agent.Call(r.Context(), op, nil, &out); err != nil {
			s.agentError(w, op, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}
}

// handleDiskSMART membaca SMART satu disk. Nama disk diperiksa lagi oleh
// sapod (harus ada di daftar disk); di sini hanya diteruskan sebagai data.
func (s *Server) handleDiskSMART(w http.ResponseWriter, r *http.Request) {
	params := proto.SMARTParams{Device: r.PathValue("name")}
	var out json.RawMessage
	if err := s.agent.Call(r.Context(), proto.OpStorageSMART, params, &out); err != nil {
		s.agentError(w, proto.OpStorageSMART, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *Server) agentError(w http.ResponseWriter, op string, err error) {
	var pe *proto.Error
	if errors.As(err, &pe) {
		status := http.StatusInternalServerError
		switch pe.Code {
		case proto.ErrInvalidParams:
			status = http.StatusBadRequest
		case proto.ErrNotFound:
			status = http.StatusNotFound
		case proto.ErrUnavailable:
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, pe.Code, pe.Message)
		return
	}
	if s.log != nil {
		s.log.Error("sapod tidak bisa dihubungi", "op", op, "err", err)
	}
	writeError(w, http.StatusServiceUnavailable, "agent_unavailable", "layanan sistem (sapod) tidak bisa dihubungi")
}

// staticHandler menyajikan panel dari web_dir dengan fallback ke index.html.
func (s *Server) staticHandler() http.Handler {
	root := s.cfg.API.WebDir
	files := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "metode tidak diizinkan")
			return
		}
		clean := path.Clean("/" + r.URL.Path)
		// File tersembunyi (.env, .git, dll.) tidak pernah disajikan.
		if strings.Contains(clean, "/.") {
			http.NotFound(w, r)
			return
		}
		full := filepath.Join(root, filepath.FromSlash(clean))
		if st, err := os.Stat(full); err == nil && !st.IsDir() {
			if strings.HasPrefix(clean, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		index := filepath.Join(root, "index.html")
		if _, err := os.Stat(index); err != nil {
			http.Error(w, "Panel belum dibangun. Jalankan: make web", http.StatusNotFound)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	})
}

// decodeJSON membaca badan permintaan JSON (maks. 64 KB, tanpa kolom asing).
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "isi permintaan tidak valid")
		return false
	}
	// Tidak boleh ada apa pun setelah satu nilai JSON.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "isi permintaan tidak valid")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeErrorDetail(w http.ResponseWriter, status int, code, msg string, detail map[string]any) {
	e := map[string]any{"code": code, "message": msg}
	for k, v := range detail {
		e[k] = v
	}
	writeJSON(w, status, map[string]any{"error": e})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
