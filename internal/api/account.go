package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mehuli-id/sapo/internal/auth"
)

// --- Keamanan akun: kode cadangan dan pendaftaran ulang 2FA ------------------
//
// Mengganti 2FA atau kode cadangan sama dengan mengambil alih akun, jadi
// selalu meminta password lagi (sesi yang dicuri saja tidak cukup). Password
// salah dihitung di batas tersendiri (reauth:<user>), bukan batas login: orang
// lain di jaringan yang sengaja mengunci login akun tidak boleh menghalangi
// pemilik yang sedang login memutar 2FA atau kode cadangan saat insiden.

// reauth memeriksa password user yang sedang login. Bila gagal, jawaban
// sudah ditulis dan ok bernilai false.
func (s *Server) reauth(w http.ResponseWriter, r *http.Request, password string) (auth.User, bool) {
	return s.reauthField(w, r, password, "password")
}

// reauthField sama dengan reauth; field adalah nama isian password di
// panel, untuk menandai isian yang salah.
func (s *Server) reauthField(w http.ResponseWriter, r *http.Request, password, field string) (auth.User, bool) {
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
	userKey := "reauth:" + user.Username
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
		writeErrorDetail(w, http.StatusUnauthorized, "bad_password", "password salah", map[string]any{"field": field})
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
	ended := s.endOtherSessions(r, user.Username)
	s.record(r, user.Username, "account.recovery_codes", true, map[string]string{"other_sessions_ended": strconv.Itoa(ended)})
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes, "other_sessions_ended": ended})
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
	ended := s.endOtherSessions(r, sess.Username)
	s.record(r, sess.Username, "account.totp", true, map[string]string{"other_sessions_ended": strconv.Itoa(ended)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "other_sessions_ended": ended})
}

// endOtherSessions mengakhiri sesi lain milik user (perangkat lain atau sesi
// yang mungkin dicuri); sesi yang sedang dipakai tetap berlaku.
func (s *Server) endOtherSessions(r *http.Request, username string) int {
	_, tok, _ := s.currentSession(r)
	return s.sessions.DeleteUserExcept(username, tok)
}

// --- Password aplikasi (WebDAV, Sapo Drive) ----------------------------------

type appPasswordView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// handleAppPasswords: password aplikasi milik pengguna sesi ini (tanpa
// hash).
func (s *Server) handleAppPasswords(w http.ResponseWriter, r *http.Request) {
	sess, _, _ := s.currentSession(r)
	u, _ := s.store.Get(sess.Username)
	out := []appPasswordView{}
	for _, ap := range u.AppPasswords {
		out = append(out, appPasswordView{ID: ap.ID, Name: ap.Name, CreatedAt: ap.CreatedAt, LastUsedAt: ap.LastUsedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAppPasswordCreate membuat password aplikasi baru (password akun
// diminta lagi). Password-nya hanya dikembalikan sekali ini.
func (s *Server) handleAppPasswordCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !auth.ValidAppPasswordName(req.Name) {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "nama perangkat tidak valid", map[string]any{"field": "name"})
		return
	}
	user, ok := s.reauth(w, r, req.Password)
	if !ok {
		return
	}
	secret, ap, err := auth.NewAppPassword(req.Name, s.now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal membuat password aplikasi")
		return
	}
	err = s.store.AddAppPassword(user.Username, ap)
	s.record(r, user.Username, "account.app_password_create", err == nil, map[string]string{"id": ap.ID, "name": auditText(ap.Name)})
	switch {
	case errors.Is(err, auth.ErrAppPasswordLimit):
		writeError(w, http.StatusConflict, "app_password_limit", "password aplikasi sudah terlalu banyak; cabut yang tidak dipakai")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan password aplikasi")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": ap.ID, "name": ap.Name, "password": secret})
}

// handleAppPasswordDelete mencabut satu password aplikasi milik sendiri.
func (s *Server) handleAppPasswordDelete(w http.ResponseWriter, r *http.Request) {
	sess, _, _ := s.currentSession(r)
	id := r.PathValue("id")
	name, err := s.store.RevokeAppPassword(sess.Username, id)
	if errors.Is(err, auth.ErrAppPasswordNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "password aplikasi tidak ditemukan")
		return
	}
	s.record(r, sess.Username, "account.app_password_revoke", err == nil, map[string]string{"id": auditText(id), "name": auditText(name)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal mencabut password aplikasi")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
