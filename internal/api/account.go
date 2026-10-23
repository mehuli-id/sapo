package api

import (
	"net/http"
	"time"

	"github.com/mehuli-id/sapo/internal/auth"
)

// --- Keamanan akun: kode cadangan dan pendaftaran ulang 2FA ------------------
//
// Mengganti 2FA atau kode cadangan sama dengan mengambil alih akun, jadi
// selalu meminta password lagi (sesi yang dicuri saja tidak cukup). Password
// salah dihitung di batas percobaan login akun yang sama.

// reauth memeriksa password user yang sedang login. Bila gagal, jawaban
// sudah ditulis dan ok bernilai false.
func (s *Server) reauth(w http.ResponseWriter, r *http.Request, password string) (auth.User, bool) {
	sess, _, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "silakan login")
		return auth.User{}, false
	}
	user, exists := s.store.Get(sess.Username)
	if !exists {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "silakan login")
		return auth.User{}, false
	}
	userKey := "user:" + user.Username
	if ok, rem := s.limiter.Begin(userKey); !ok {
		s.writeLocked(w, rem)
		return auth.User{}, false
	}
	var passOK bool
	if !s.withHashSlot(r.Context(), func() { passOK, _ = auth.VerifyPassword(password, user.PasswordHash) }) {
		s.limiter.Refund(userKey)
		writeBusy(w)
		return auth.User{}, false
	}
	if !passOK {
		s.record(r, user.Username, "account.reauth", false, nil)
		writeErrorDetail(w, http.StatusUnauthorized, "bad_password", "password salah", map[string]any{"field": "password"})
		return auth.User{}, false
	}
	s.limiter.Refund(userKey)
	return user, true
}

// handleRecoveryCodes membuat kode cadangan baru; semua kode lama tidak berlaku.
func (s *Server) handleRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	user, ok := s.reauth(w, r, req.Password)
	if !ok {
		return
	}
	if user.TOTPSecret == "" {
		writeError(w, http.StatusConflict, "totp_not_enabled", "2FA belum aktif untuk akun ini")
		return
	}
	codes, hashes, err := auth.NewRecoveryCodes(s.cfg.Auth.RecoveryCodeCount)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal membuat kode cadangan")
		return
	}
	if err := s.store.SetRecoveryCodes(user.Username, hashes); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan kode cadangan")
		return
	}
	s.record(r, user.Username, "account.recovery_codes", true, nil)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// handleTOTPBegin memulai pendaftaran ulang aplikasi autentikator (misalnya
// HP baru). Rahasia lama tetap berlaku sampai rahasia baru dikonfirmasi.
func (s *Server) handleTOTPBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	user, ok := s.reauth(w, r, req.Password)
	if !ok {
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
	s.reenroll.mu.Lock()
	for k, e := range s.reenroll.entries {
		// Satu pendaftaran ulang per user: yang lama diganti.
		if now.After(e.expires) || e.client == user.Username {
			delete(s.reenroll.entries, k)
		}
	}
	s.reenroll.entries[token] = pendingEntry{secret: secret, client: user.Username, expires: now.Add(time.Duration(s.cfg.Auth.SetupTOTPExpiryMinutes) * time.Minute)}
	s.reenroll.mu.Unlock()
	writeJSON(w, http.StatusOK, setupTOTPResponse{Token: token, Secret: secret, URI: auth.TOTPURI(s.productName(), user.Username, secret), RecoveryCodes: []string{}})
}

// handleTOTPConfirm mengganti rahasia 2FA setelah kode dari aplikasi baru
// terbukti benar. Hanya pendaftaran milik user yang sedang login yang berlaku.
func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
		Code  string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "silakan login")
		return
	}
	s.reenroll.mu.Lock()
	pending, found := s.reenroll.entries[req.Token]
	s.reenroll.mu.Unlock()
	if !found || pending.client != sess.Username || s.now().After(pending.expires) {
		writeErrorDetail(w, http.StatusBadRequest, "totp_expired", "pendaftaran 2FA kedaluwarsa, ulangi dari awal", map[string]any{"field": "code"})
		return
	}
	totpKey := "totp:" + sess.Username
	if ok, rem := s.totpLimiter.Begin(totpKey); !ok {
		s.writeLocked(w, rem)
		return
	}
	step, valid := auth.VerifyTOTP(pending.secret, req.Code, s.now(), 0)
	if !valid {
		s.record(r, sess.Username, "account.totp", false, map[string]string{"reason": "bad_totp"})
		writeErrorDetail(w, http.StatusBadRequest, "totp_invalid", "kode 2FA salah", map[string]any{"field": "code"})
		return
	}
	if err := s.store.SetTOTP(sess.Username, pending.secret, step); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan 2FA")
		return
	}
	s.totpLimiter.Reset(totpKey)
	s.reenroll.mu.Lock()
	delete(s.reenroll.entries, req.Token)
	s.reenroll.mu.Unlock()
	s.record(r, sess.Username, "account.totp", true, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
