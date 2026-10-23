package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/audit"
	"github.com/mehuli-id/sapo/internal/auth"
)

type ctxKey int

const sessionKey ctxKey = 1

// maxPendingSetup membatasi jumlah pendaftaran 2FA yang menunggu selama
// setup awal (satu per browser yang sedang membuka wizard).
const maxPendingSetup = 16

// pendingTOTP menyimpan rahasia 2FA yang sedang didaftarkan saat setup awal.
// Setiap browser mendapat token sendiri, sehingga klien lain di LAN tidak
// bisa menimpa pendaftaran milik admin yang sedang mengisi wizard.
type pendingTOTP struct {
	mu      sync.Mutex
	entries map[string]pendingEntry
}

type pendingEntry struct {
	secret  string
	expires time.Time
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) currentSession(r *http.Request) (auth.Session, string, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return auth.Session{}, "", false
	}
	sess, ok := s.sessions.Get(c.Value)
	if !ok {
		return auth.Session{}, "", false
	}
	// User yang sudah dihapus tidak boleh tetap masuk.
	if _, exists := s.store.Get(sess.Username); !exists {
		s.sessions.Delete(c.Value)
		return auth.Session{}, "", false
	}
	return sess, c.Value, true
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, _, ok := s.currentSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "silakan login")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"setup_required": s.store.SetupRequired(), "authenticated": false}
	if sess, _, ok := s.currentSession(r); ok {
		resp["authenticated"] = true
		resp["user"] = map[string]any{"username": sess.Username, "role": sess.Role}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) record(r *http.Request, actor, action string, ok bool, detail map[string]string) {
	if err := s.audit.Record(audit.Event{Time: s.now().UTC(), Actor: actor, Action: action, IP: clientIP(r), OK: ok, Detail: detail}); err != nil && s.log != nil {
		s.log.Error("gagal menulis log audit", "err", err)
	}
}

// validUsername memeriksa bentuk nama sebelum dipakai untuk apa pun (lookup,
// kunci limiter, log). Nama yang tidak valid tidak pernah dicatat apa adanya.
func (s *Server) validUsername(name string) bool {
	return len(name) <= 64 && s.userRe.MatchString(name)
}

// withHashSlot membatasi hash Argon2 yang berjalan bersamaan agar permintaan
// login massal tidak menghabiskan RAM.
func (s *Server) withHashSlot(ctx context.Context, fn func()) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case s.hashSlots <- struct{}{}:
		defer func() { <-s.hashSlots }()
		fn()
		return true
	case <-ctx.Done():
		return false
	}
}

func writeBusy(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "busy", "server sedang sibuk, coba lagi sebentar")
}

// --- Setup awal -------------------------------------------------------------

type setupTOTPResponse struct {
	Token  string `json:"token"`
	Secret string `json:"secret"`
	URI    string `json:"uri"`
}

// handleSetupTOTP membuat rahasia 2FA untuk admin pertama. Hanya berlaku
// selama setup belum selesai.
func (s *Server) handleSetupTOTP(w http.ResponseWriter, r *http.Request) {
	if !s.store.SetupRequired() {
		writeError(w, http.StatusConflict, "setup_done", "setup awal sudah selesai")
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal membuat rahasia 2FA")
		return
	}
	token, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal membuat rahasia 2FA")
		return
	}
	now := s.now()
	s.setup.mu.Lock()
	for k, e := range s.setup.entries {
		if now.After(e.expires) {
			delete(s.setup.entries, k)
		}
	}
	if len(s.setup.entries) >= maxPendingSetup {
		s.setup.mu.Unlock()
		writeBusy(w)
		return
	}
	s.setup.entries[token] = pendingEntry{secret: secret, expires: now.Add(time.Duration(s.cfg.Auth.SetupTOTPExpiryMinutes) * time.Minute)}
	s.setup.mu.Unlock()

	account := strings.ToLower(strings.TrimSpace(req.Username))
	if !s.validUsername(account) {
		account = "admin"
	}
	writeJSON(w, http.StatusOK, setupTOTPResponse{Token: token, Secret: secret, URI: auth.TOTPURI(s.productName(), account, secret)})
}

type setupRequest struct {
	Language        string `json:"language"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	PasswordConfirm string `json:"password_confirm"`
	TOTPCode        string `json:"totp_code"`
	TOTPToken       string `json:"totp_token"`
}

func (s *Server) validateNewUser(username, password, confirm string) (string, string) {
	switch {
	case !s.validUsername(username):
		return "username_invalid", "nama pengguna tidak sesuai aturan"
	case slices.Contains(s.cfg.Auth.ReservedUsernames, username):
		return "username_reserved", "nama pengguna ini dipakai sistem"
	case utf8.RuneCountInString(password) < s.cfg.Auth.PasswordMinLength:
		return "password_too_short", "password terlalu pendek"
	case len(password) > 1024:
		return "password_too_long", "password terlalu panjang"
	case strings.EqualFold(password, username):
		return "password_is_username", "password tidak boleh sama dengan nama pengguna"
	case password != confirm:
		return "password_mismatch", "konfirmasi password tidak sama"
	}
	return "", ""
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.store.SetupRequired() {
		writeError(w, http.StatusConflict, "setup_done", "setup awal sudah selesai")
		return
	}
	var req setupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(strings.ToLower(req.Username))
	if code, msg := s.validateNewUser(req.Username, req.Password, req.PasswordConfirm); code != "" {
		writeErrorDetail(w, http.StatusBadRequest, code, msg, map[string]any{"field": fieldFor(code)})
		return
	}
	if req.Language == "" {
		req.Language = s.cfg.API.DefaultLanguage
	}

	var secret string
	var step int64
	if s.cfg.Auth.RequireTOTPForAdmin {
		s.setup.mu.Lock()
		pending, found := s.setup.entries[req.TOTPToken]
		s.setup.mu.Unlock()
		if !found || s.now().After(pending.expires) {
			writeErrorDetail(w, http.StatusBadRequest, "totp_expired", "kode 2FA kedaluwarsa, ulangi pendaftaran 2FA", map[string]any{"field": "totp_code"})
			return
		}
		var ok bool
		if step, ok = auth.VerifyTOTP(pending.secret, req.TOTPCode, s.now(), 0); !ok {
			writeErrorDetail(w, http.StatusBadRequest, "totp_invalid", "kode 2FA salah", map[string]any{"field": "totp_code"})
			return
		}
		secret = pending.secret
	}

	var hash string
	var hashErr error
	if !s.withHashSlot(r.Context(), func() { hash, hashErr = auth.HashPassword(req.Password, s.argon) }) {
		writeBusy(w)
		return
	}
	if hashErr != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan password")
		return
	}
	admin := auth.User{Username: req.Username, Role: auth.RoleAdmin, PasswordHash: hash, TOTPSecret: secret, TOTPLastStep: step, CreatedAt: s.now().UTC()}
	if err := s.store.CompleteSetup(admin, req.Language, s.now().UTC()); err != nil {
		if errors.Is(err, auth.ErrSetupDone) {
			writeError(w, http.StatusConflict, "setup_done", "setup awal sudah selesai")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan akun")
		return
	}
	s.setup.mu.Lock()
	clear(s.setup.entries)
	s.setup.mu.Unlock()
	s.record(r, admin.Username, "setup.complete", true, map[string]string{"language": req.Language})

	tok, err := s.sessions.Create(admin.Username, admin.Role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal membuat sesi")
		return
	}
	s.setCookie(w, r, tok, 0)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": map[string]any{"username": admin.Username, "role": admin.Role}})
}

func fieldFor(code string) string {
	switch {
	case strings.HasPrefix(code, "username"):
		return "username"
	case code == "password_mismatch":
		return "password_confirm"
	case strings.HasPrefix(code, "password"):
		return "password"
	}
	return ""
}

// --- Login & logout ---------------------------------------------------------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TOTPCode string `json:"totp_code"`
}

func (s *Server) writeLocked(w http.ResponseWriter, rem time.Duration) {
	writeErrorDetail(w, http.StatusTooManyRequests, "locked", "terlalu banyak percobaan, coba lagi nanti",
		map[string]any{"retry_after_seconds": int(math.Ceil(rem.Seconds()))})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(strings.ToLower(req.Username))
	valid := s.validUsername(req.Username)
	actor := req.Username
	if !valid {
		actor = "(nama tidak valid)"
	}
	ipKey := "ip:" + clientIP(r)
	userKey := "user:" + req.Username

	// Percobaan dipesan lebih dulu (atomik), sehingga permintaan paralel
	// tidak bisa melewati batas. Kunci per IP selalu berlaku.
	if ok, rem := s.limiter.Begin(ipKey); !ok {
		if s.log != nil {
			s.log.Warn("login ditolak: IP terkunci", "ip", clientIP(r))
		}
		s.writeLocked(w, rem)
		return
	}
	// Kunci per nama pengguna mencegah tebakan dari banyak IP. Agar orang
	// lain tidak bisa mengunci admin selamanya, admin yang memakai 2FA tetap
	// boleh masuk bila password DAN kode 2FA-nya benar.
	userLocked, userRem := false, time.Duration(0)
	if valid {
		ok, rem := s.limiter.Begin(userKey)
		userLocked, userRem = !ok, rem
	}

	var user auth.User
	exists := false
	if valid {
		user, exists = s.store.Get(req.Username)
	}
	hash := s.dummy
	if exists {
		hash = user.PasswordHash
	}
	var passOK bool
	if !s.withHashSlot(r.Context(), func() { passOK, _ = auth.VerifyPassword(req.Password, hash) }) {
		writeBusy(w)
		return
	}
	if !exists || !passOK {
		s.record(r, actor, "auth.login", false, map[string]string{"reason": "bad_credentials"})
		writeError(w, http.StatusUnauthorized, "bad_credentials", "nama pengguna atau password salah")
		return
	}
	if userLocked && user.TOTPSecret == "" {
		s.record(r, user.Username, "auth.login", false, map[string]string{"reason": "locked"})
		s.writeLocked(w, userRem)
		return
	}

	if user.TOTPSecret != "" {
		if strings.TrimSpace(req.TOTPCode) == "" {
			writeError(w, http.StatusUnauthorized, "totp_required", "masukkan kode dari aplikasi autentikator")
			return
		}
		step, valid := auth.VerifyTOTP(user.TOTPSecret, req.TOTPCode, s.now(), user.TOTPLastStep)
		if !valid {
			s.record(r, user.Username, "auth.login", false, map[string]string{"reason": "bad_totp"})
			writeError(w, http.StatusUnauthorized, "totp_invalid", "kode 2FA salah")
			return
		}
		// Compare-and-set: dua login bersamaan dengan kode yang sama, hanya
		// satu yang berhasil.
		if err := s.store.ConsumeTOTPStep(user.Username, step); err != nil {
			if errors.Is(err, auth.ErrTOTPReused) {
				s.record(r, user.Username, "auth.login", false, map[string]string{"reason": "totp_reused"})
				writeError(w, http.StatusUnauthorized, "totp_invalid", "kode 2FA salah")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan status 2FA")
			return
		}
	}

	s.limiter.Reset(ipKey)
	s.limiter.Reset(userKey)

	// Hash ulang bila parameter Argon2 di konfigurasi sudah dinaikkan.
	if auth.NeedsRehash(user.PasswordHash, s.argon) {
		s.withHashSlot(r.Context(), func() {
			if h, err := auth.HashPassword(req.Password, s.argon); err == nil {
				_ = s.store.SetPasswordHash(user.Username, h)
			}
		})
	}

	tok, err := s.sessions.Create(user.Username, user.Role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal membuat sesi")
		return
	}
	s.setCookie(w, r, tok, 0)
	s.record(r, user.Username, "auth.login", true, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": map[string]any{"username": user.Username, "role": user.Role}})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess, tok, ok := s.currentSession(r); ok {
		s.sessions.Delete(tok)
		s.record(r, sess.Username, "auth.logout", true, nil)
	}
	s.setCookie(w, r, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
