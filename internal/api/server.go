// Package api berisi sapo-api: server HTTP untuk panel dan aplikasi.
//
// sapo-api berjalan tanpa hak root dan tidak pernah menjalankan perintah
// sistem. Semua perubahan sistem diteruskan ke sapod sebagai operasi bernama.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/proto"
	"gopkg.in/yaml.v3"
)

// AgentCaller adalah kemampuan memanggil operasi sapod (diimplementasikan
// oleh agent.Client; di test diganti tiruan).
type AgentCaller interface {
	Call(ctx context.Context, op string, params any, out any) error
}

// Server adalah handler HTTP sapo-api.
type Server struct {
	cfg   config.Config
	agent AgentCaller
	log   *slog.Logger
	mux   *http.ServeMux
}

// New menyusun server beserta rutenya.
func New(cfg config.Config, agent AgentCaller, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, agent: agent, log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/ui/config", s.handleUIConfig)
	s.mux.HandleFunc("GET /api/v1/system/info", s.proxyRead(proto.OpSystemInfo))
	s.mux.HandleFunc("GET /api/v1/features", s.proxyRead(proto.OpFeaturesList))
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "endpoint tidak ada")
	})
	s.mux.Handle("/", s.staticHandler())
	return s
}

// ServeHTTP menerapkan header keamanan dan log ke setiap permintaan.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(rec, r)
	if s.log != nil && strings.HasPrefix(r.URL.Path, "/api/") {
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "durasi", time.Since(start))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	agentOK := s.agent.Call(r.Context(), proto.OpSystemInfo, nil, nil) == nil
	status := http.StatusOK
	if !agentOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"status": map[bool]string{true: "ok", false: "degraded"}[agentOK], "agent": agentOK})
}

// handleUIConfig memberi panel nilai default dan branding dari konfigurasi
// (bukan dari kode panel). Bagian "updates" di branding tidak dikirim.
func (s *Server) handleUIConfig(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]any{
		"default_mode":     s.cfg.UI.DefaultMode,
		"default_language": s.cfg.API.DefaultLanguage,
	}
	if b, err := loadBranding(s.cfg.API.BrandingFile); err == nil {
		resp["branding"] = b
	} else if s.log != nil {
		s.log.Warn("branding tidak terbaca", "file", s.cfg.API.BrandingFile, "err", err)
	}
	writeJSON(w, http.StatusOK, resp)
}

type branding struct {
	Product struct {
		Name     string            `yaml:"name" json:"name"`
		FullName string            `yaml:"full_name" json:"full_name"`
		Vendor   string            `yaml:"vendor" json:"vendor"`
		Website  string            `yaml:"website" json:"website"`
		Tagline  map[string]string `yaml:"tagline" json:"tagline"`
	} `yaml:"product" json:"product"`
	Theme struct {
		Radius string            `yaml:"radius" json:"radius,omitempty"`
		Light  map[string]string `yaml:"light" json:"light,omitempty"`
		Dark   map[string]string `yaml:"dark" json:"dark,omitempty"`
	} `yaml:"theme" json:"theme"`
}

var cssValue = regexp.MustCompile(`^[a-zA-Z0-9 .,%()/#-]{1,64}$`)
var cssName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

func loadBranding(path string) (*branding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b branding
	if err := yaml.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	// Nilai tema disisipkan sebagai CSS di panel: hanya izinkan karakter aman.
	check := func(m map[string]string) error {
		for k, v := range m {
			if !cssName.MatchString(k) || !cssValue.MatchString(v) {
				return fmt.Errorf("nilai tema tidak valid: %s", k)
			}
		}
		return nil
	}
	if err := check(b.Theme.Light); err != nil {
		return nil, err
	}
	if err := check(b.Theme.Dark); err != nil {
		return nil, err
	}
	if b.Theme.Radius != "" && !cssValue.MatchString(b.Theme.Radius) {
		return nil, fmt.Errorf("radius tidak valid")
	}
	return &b, nil
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

func (s *Server) agentError(w http.ResponseWriter, op string, err error) {
	var pe *proto.Error
	if errors.As(err, &pe) {
		status := http.StatusInternalServerError
		if pe.Code == proto.ErrInvalidParams {
			status = http.StatusBadRequest
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

// EnsureLoopback menolak alamat listen selain localhost. Autentikasi belum
// ada di tahap ini, jadi sapo-api tidak boleh terbuka ke jaringan.
func EnsureLoopback(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("api.listen tidak valid: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("api.listen %q ditolak: sebelum autentikasi tersedia, sapo-api hanya boleh mendengarkan localhost", listen)
	}
	return nil
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

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
