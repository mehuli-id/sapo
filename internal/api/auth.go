package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
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
	client  string
	expires time.Time
	// Hash kode cadangan yang ditampilkan bersama QR; disimpan saat setup selesai.
	recovery []string
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
		Name:     sessionCookieName(r),
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) currentSession(r *http.Request) (auth.Session, string, bool) {
	c, err := r.Cookie(sessionCookieName(r))
	if err != nil {
		return auth.Session{}, "", false
	}
	sess, ok := s.sessions.Get(c.Value)
	if !ok {
		return auth.Session{}, "", false
	}
	// User yang sudah dihapus atau dinonaktifkan tidak boleh tetap masuk.
	if u, exists := s.store.Get(sess.Username); !exists || u.Disabled {
		s.sessions.Delete(c.Value)
		return auth.Session{}, "", false
	}
	return sess, c.Value, true
}

// requireAuth: harus login, dan akun tidak sedang menunggu langkah wajib
// (ganti password dari admin, atau 2FA untuk administrator).
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return s.authorize(next, false)
}

// requireAuthPending: harus login; boleh dipakai selama langkah wajib akun
// belum selesai (justru untuk menyelesaikannya).
func (s *Server) requireAuthPending(next http.Handler) http.Handler {
	return s.authorize(next, true)
}

func (s *Server) authorize(next http.Handler, allowPending bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, _, ok := s.currentSession(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "silakan login")
			return
		}
		if !allowPending {
			if u, _ := s.store.Get(sess.Username); len(s.pendingSteps(u)) > 0 {
				writeError(w, http.StatusForbidden, "account_setup_required", "selesaikan dulu langkah wajib akun")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	})
}

// pendingSteps adalah langkah wajib sebelum akun boleh memakai panel:
// "password" (password dibuat atau di-reset admin) dan "totp"
// (administrator tanpa 2FA, bila auth.require_totp_for_admin).
func (s *Server) pendingSteps(u auth.User) []string {
	steps := []string{}
	if u.MustChangePassword {
		steps = append(steps, "password")
	}
	if u.Role == auth.RoleAdmin && s.cfg.Auth.RequireTOTPForAdmin && u.TOTPSecret == "" {
		steps = append(steps, "totp")
	}
	return steps
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"setup_required": s.store.SetupRequired(), "authenticated": false}
	if sess, _, ok := s.currentSession(r); ok {
		resp["authenticated"] = true
		u, _ := s.store.Get(sess.Username)
		// Peran dari data akun (bisa berubah setelah login).
		user := map[string]any{"username": sess.Username, "role": u.Role, "pending": s.pendingSteps(u)}
		if u.TOTPSecret != "" {
			// Panel memberi peringatan bila kode cadangan habis atau tinggal sedikit.
			user["recovery_codes_left"] = len(u.RecoveryCodes)
			user["recovery_codes_low"] = len(u.RecoveryCodes) <= s.cfg.Auth.RecoveryCodesLow
		}
		resp["user"] = user
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
	// Kode cadangan 2FA (hanya ditampilkan sekali).
	RecoveryCodes []string `json:"recovery_codes"`
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
	codes, hashes, err := auth.NewRecoveryCodes(s.cfg.Auth.RecoveryCodeCount)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal membuat kode cadangan")
		return
	}
	now := s.now()
	client := clientNet(r)
	s.setup.mu.Lock()
	for k, e := range s.setup.entries {
		// Satu pendaftaran per klien: yang lama dari klien yang sama diganti,
		// sehingga satu perangkat tidak bisa memenuhi semua slot.
		if now.After(e.expires) || e.client == client {
			delete(s.setup.entries, k)
		}
	}
	if len(s.setup.entries) >= maxPendingSetup {
		s.setup.mu.Unlock()
		writeBusy(w)
		return
	}
	s.setup.entries[token] = pendingEntry{secret: secret, client: client, recovery: hashes, expires: now.Add(time.Duration(s.cfg.Auth.SetupTOTPExpiryMinutes) * time.Minute)}
	s.setup.mu.Unlock()

	account := strings.ToLower(strings.TrimSpace(req.Username))
	if !s.validUsername(account) {
		account = "admin"
	}
	writeJSON(w, http.StatusOK, setupTOTPResponse{Token: token, Secret: secret, URI: auth.TOTPURI(s.productName(), account, secret), RecoveryCodes: codes})
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
	// Nama pengguna dan grup satu ruang nama (nantinya juga akun dan grup
	// sistem): nama grup sistem, grup Sapo, dan akun sistem tidak boleh dipakai.
	case slices.Contains(s.cfg.Auth.ReservedUsernames, username) || slices.Contains(s.cfg.Auth.ReservedGroupNames, username):
		return "username_reserved", "nama pengguna ini dipakai sistem"
	case s.groupExists(username):
		return "username_taken", "nama ini sudah dipakai grup"
	}
	return s.validatePassword(username, password, confirm)
}

// validatePassword memeriksa aturan password saja (untuk akun yang sudah
// ada, agar perubahan daftar nama yang dicadangkan tidak mengunci pemiliknya).
func (s *Server) validatePassword(username, password, confirm string) (string, string) {
	switch {
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
	var recovery []string
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
		recovery = pending.recovery
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
	admin := auth.User{Username: req.Username, Role: auth.RoleAdmin, PasswordHash: hash, TOTPSecret: secret, TOTPLastStep: step, RecoveryCodes: recovery, CreatedAt: s.now().UTC()}
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
	code := strings.TrimSpace(req.TOTPCode)
	valid := s.validUsername(req.Username)
	actor := req.Username
	if !valid {
		actor = "(nama tidak valid)"
	}
	var user auth.User
	exists := false
	if valid {
		user, exists = s.store.Get(req.Username)
	}
	hasTOTP := exists && user.TOTPSecret != ""
	usedRecovery, recoveryLeft := false, 0

	// 1. Batas per jaringan klien (IPv4 per alamat, IPv6 per /64, karena satu
	//    perangkat bisa memakai banyak alamat IPv6). Dipesan atomik.
	ipKey := "ip:" + clientNet(r)
	if ok, rem := s.limiter.Begin(ipKey); !ok {
		if s.log != nil {
			s.log.Warn("login ditolak: jaringan klien terkunci", "ip", clientIP(r))
		}
		s.writeLocked(w, rem)
		return
	}
	// 2. Batas per akun, hanya untuk akun yang benar-benar ada (nama acak
	//    tidak membuat entri, sehingga tidak bisa membanjiri tabel).
	userKey := "user:" + req.Username
	userLocked := false
	if exists {
		ok, _ := s.limiter.Begin(userKey)
		userLocked = !ok
	}
	totpKey := "totp:" + req.Username

	// Saat akun terkunci, hanya satu permintaan berisi password DAN kode 2FA
	// yang diterima, dan semua kegagalan dijawab sama. Penyerang tidak bisa
	// mengetahui apakah password-nya benar. Akun tanpa 2FA cukup ditolak.
	if userLocked {
		if !hasTOTP {
			s.record(r, user.Username, "auth.login", false, map[string]string{"reason": "locked"})
			s.writeLocked(w, s.limiter.Locked(userKey))
			return
		}
		if code == "" {
			writeError(w, http.StatusTooManyRequests, "locked_need_code", "akun sedang dilindungi; masukkan password dan kode 2FA sekaligus")
			return
		}
	}
	// Batas kode 2FA salah per akun diperiksa sebelum password, agar
	// jawabannya tidak bergantung pada benar/salahnya password.
	if hasTOTP {
		if rem := s.totpLimiter.Locked(totpKey); rem > 0 {
			s.writeLocked(w, rem)
			return
		}
	}

	hash := s.dummy
	if exists {
		hash = user.PasswordHash
	}
	var passOK bool
	if !s.withHashSlot(r.Context(), func() { passOK, _ = auth.VerifyPassword(req.Password, hash) }) {
		// Server sibuk bukan kesalahan pengguna: percobaan dikembalikan.
		s.limiter.Refund(ipKey)
		s.limiter.Refund(userKey)
		writeBusy(w)
		return
	}
	if !exists || !passOK {
		s.record(r, actor, "auth.login", false, map[string]string{"reason": "bad_credentials"})
		writeError(w, http.StatusUnauthorized, "bad_credentials", "nama pengguna atau password salah")
		return
	}
	// Password benar tidak pernah dihitung sebagai percobaan gagal.
	s.limiter.Refund(ipKey)
	s.limiter.Refund(userKey)

	if hasTOTP {
		if code == "" {
			writeError(w, http.StatusUnauthorized, "totp_required", "masukkan kode dari aplikasi autentikator")
			return
		}
		// Kode salah dihitung di penghitung 2FA milik akun (bukan per IP),
		// sehingga salah ketik tidak mengunci jaringan admin.
		badCode := func(reason string) {
			s.record(r, user.Username, "auth.login", false, map[string]string{"reason": reason})
			if userLocked {
				writeError(w, http.StatusUnauthorized, "bad_credentials", "nama pengguna atau password salah")
				return
			}
			writeError(w, http.StatusUnauthorized, "totp_invalid", "kode 2FA salah")
		}
		if ok, rem := s.totpLimiter.Begin(totpKey); !ok {
			s.record(r, user.Username, "auth.login", false, map[string]string{"reason": "totp_locked"})
			s.writeLocked(w, rem)
			return
		}
		if auth.LooksLikeRecoveryCode(code) {
			// Kode cadangan (HP autentikator tidak tersedia): sekali pakai,
			// dihitung di penghitung kode 2FA salah yang sama.
			left, err := s.store.ConsumeRecoveryCode(user.Username, code)
			if err != nil {
				if errors.Is(err, auth.ErrRecoveryInvalid) {
					badCode("bad_recovery_code")
					return
				}
				s.totpLimiter.Refund(totpKey) // gagal menyimpan bukan tebakan salah
				writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan status 2FA")
				return
			}
			usedRecovery, recoveryLeft = true, left
		} else {
			step, valid := auth.VerifyTOTP(user.TOTPSecret, code, s.now(), user.TOTPLastStep)
			if !valid {
				badCode("bad_totp")
				return
			}
			// Compare-and-set: dua login bersamaan dengan kode yang sama, hanya
			// satu yang berhasil.
			if err := s.store.ConsumeTOTPStep(user.Username, user.TOTPSecret, step); err != nil {
				if errors.Is(err, auth.ErrTOTPReused) {
					badCode("totp_reused")
					return
				}
				s.totpLimiter.Refund(totpKey)
				writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan status 2FA")
				return
			}
		}
	}

	// Baru diberi tahu setelah password (dan kode 2FA) terbukti benar, agar
	// jawaban saat akun terkunci tetap seragam.
	if user.Disabled {
		s.record(r, user.Username, "auth.login", false, map[string]string{"reason": "disabled"})
		writeError(w, http.StatusForbidden, "account_disabled", "akun ini dinonaktifkan administrator")
		return
	}

	s.limiter.Reset(ipKey)
	s.limiter.Reset(userKey)
	s.totpLimiter.Reset(totpKey)

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
	_ = s.store.RecordLogin(user.Username, s.now().UTC())
	s.setCookie(w, r, tok, 0)
	if usedRecovery {
		// Dicatat terpisah: pemakaian kode cadangan perlu diketahui admin.
		s.record(r, user.Username, "auth.login", true, map[string]string{"method": "recovery_code", "recovery_codes_left": strconv.Itoa(recoveryLeft)})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": map[string]any{"username": user.Username, "role": user.Role}, "recovery_codes_left": recoveryLeft})
		return
	}
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
