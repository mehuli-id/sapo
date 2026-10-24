package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/auth"
)

// --- Pengguna dan grup (admin) -----------------------------------------------
//
// Tindakan yang memberi hak atau mencabut akses meminta password admin lagi:
// membuat user, mengubah peran, me-reset password, dan menghapus user.
// Mengubah peran, menonaktifkan, me-reset password, atau menghapus user
// langsung mengakhiri semua sesinya. Admin tidak bisa menonaktifkan,
// menurunkan, atau menghapus dirinya sendiri, dan NAS selalu punya minimal
// satu administrator aktif (dijaga di auth.Store).

type userView struct {
	Username           string     `json:"username"`
	Role               auth.Role  `json:"role"`
	Description        string     `json:"description"`
	Disabled           bool       `json:"disabled"`
	Groups             []string   `json:"groups"`
	TwoFactor          bool       `json:"two_factor"`
	MustChangePassword bool       `json:"must_change_password"`
	CreatedAt          time.Time  `json:"created_at"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
}

func viewOf(u auth.User) userView {
	groups := u.Groups
	if groups == nil {
		groups = []string{}
	}
	return userView{
		Username: u.Username, Role: u.Role, Description: u.Description, Disabled: u.Disabled, Groups: groups,
		TwoFactor: u.TOTPSecret != "", MustChangePassword: u.MustChangePassword, CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt,
	}
}

type groupView struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Members     []string  `json:"members"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Server) handleUsers(w http.ResponseWriter, _ *http.Request) {
	out := []userView{}
	for _, u := range s.store.Users() {
		out = append(out, viewOf(u))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGroups(w http.ResponseWriter, _ *http.Request) {
	users := s.store.Users()
	out := []groupView{}
	for _, g := range s.store.Groups() {
		v := groupView{Name: g.Name, Description: g.Description, Members: []string{}, CreatedAt: g.CreatedAt}
		for _, u := range users {
			if slices.Contains(u.Groups, g.Name) {
				v.Members = append(v.Members, u.Username)
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// maxDescription membatasi keterangan user dan grup.
const maxDescription = 128

func validDescription(d string) bool {
	if utf8.RuneCountInString(d) > maxDescription || !utf8.ValidString(d) {
		return false
	}
	return !strings.ContainsFunc(d, unicode.IsControl)
}

func validGroups(groups []string) bool {
	return len(groups) <= 64
}

// userStoreError memetakan kesalahan penyimpanan ke jawaban HTTP.
func userStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrUserExists):
		writeErrorDetail(w, http.StatusConflict, "username_taken", "nama pengguna sudah dipakai", map[string]any{"field": "username"})
	case errors.Is(err, auth.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "not_found", "user tidak ditemukan")
	case errors.Is(err, auth.ErrLastAdmin):
		writeError(w, http.StatusConflict, "last_admin", "harus tetap ada minimal satu administrator aktif")
	case errors.Is(err, auth.ErrGroupExists):
		writeErrorDetail(w, http.StatusConflict, "group_taken", "nama grup sudah dipakai", map[string]any{"field": "name"})
	case errors.Is(err, auth.ErrGroupNotFound):
		writeErrorDetail(w, http.StatusBadRequest, "group_not_found", "grup tidak ditemukan", map[string]any{"field": "groups"})
	default:
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan data pengguna")
	}
}

// hashNew membuat hash password baru. ok=false: jawaban sudah ditulis.
func (s *Server) hashNew(w http.ResponseWriter, r *http.Request, password string) (string, bool) {
	var hash string
	var err error
	if !s.withHashSlot(r.Context(), func() { hash, err = auth.HashPassword(password, s.argon) }) {
		writeBusy(w)
		return "", false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal membuat hash password")
		return "", false
	}
	return hash, true
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username           string    `json:"username"`
		Password           string    `json:"password"`
		PasswordConfirm    string    `json:"password_confirm"`
		Role               auth.Role `json:"role"`
		Groups             []string  `json:"groups"`
		Description        string    `json:"description"`
		MustChangePassword bool      `json:"must_change_password"`
		AdminPassword      string    `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(strings.ToLower(req.Username))
	if code, msg := s.validateNewUser(req.Username, req.Password, req.PasswordConfirm); code != "" {
		writeErrorDetail(w, http.StatusBadRequest, code, msg, map[string]any{"field": fieldFor(code)})
		return
	}
	if !auth.ValidRole(req.Role) || !validGroups(req.Groups) || !validDescription(req.Description) {
		writeError(w, http.StatusBadRequest, "bad_request", "isi permintaan tidak valid")
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	hash, ok := s.hashNew(w, r, req.Password)
	if !ok {
		return
	}
	u := auth.User{
		Username: req.Username, Role: req.Role, PasswordHash: hash, CreatedAt: s.now().UTC(),
		Description: req.Description, Groups: req.Groups, MustChangePassword: req.MustChangePassword,
	}
	if err := s.store.CreateUser(u); err != nil {
		userStoreError(w, err)
		return
	}
	s.record(r, admin.Username, "users.create", true, map[string]string{"user": u.Username, "role": string(u.Role), "groups": strings.Join(u.Groups, ",")})
	created, _ := s.store.Get(u.Username)
	writeJSON(w, http.StatusOK, viewOf(created))
}

func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role          *auth.Role `json:"role"`
		Disabled      *bool      `json:"disabled"`
		Groups        *[]string  `json:"groups"`
		Description   *string    `json:"description"`
		AdminPassword string     `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	target, exists := s.store.Get(name)
	if !exists {
		writeError(w, http.StatusNotFound, "not_found", "user tidak ditemukan")
		return
	}
	if (req.Role != nil && !auth.ValidRole(*req.Role)) || (req.Groups != nil && !validGroups(*req.Groups)) || (req.Description != nil && !validDescription(*req.Description)) {
		writeError(w, http.StatusBadRequest, "bad_request", "isi permintaan tidak valid")
		return
	}
	sess, _, _ := s.currentSession(r)
	roleChange := req.Role != nil && *req.Role != target.Role
	disabling := req.Disabled != nil && *req.Disabled && !target.Disabled
	if name == sess.Username && (roleChange || disabling) {
		writeError(w, http.StatusConflict, "self_change", "tidak bisa mengubah peran atau menonaktifkan akun sendiri")
		return
	}
	actor := sess.Username
	if roleChange {
		admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
		if !ok {
			return
		}
		actor = admin.Username
	}
	err := s.store.UpdateUser(name, auth.UserChange{Role: req.Role, Disabled: req.Disabled, Groups: req.Groups, Description: req.Description})
	detail := map[string]string{"user": name}
	if req.Role != nil {
		detail["role"] = string(*req.Role)
	}
	if req.Disabled != nil {
		detail["disabled"] = boolText(*req.Disabled)
	}
	if req.Groups != nil {
		detail["groups"] = strings.Join(*req.Groups, ",")
	}
	if err != nil {
		detail["error"] = err.Error()
		s.record(r, actor, "users.update", false, detail)
		userStoreError(w, err)
		return
	}
	// Hak berubah atau akses dicabut: sesi lama tidak boleh tetap berlaku.
	if roleChange || disabling {
		s.sessions.DeleteUserExcept(name, "")
	}
	s.record(r, actor, "users.update", true, detail)
	updated, _ := s.store.Get(name)
	writeJSON(w, http.StatusOK, viewOf(updated))
}

func (s *Server) handleUserPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password           string `json:"password"`
		PasswordConfirm    string `json:"password_confirm"`
		MustChangePassword bool   `json:"must_change_password"`
		AdminPassword      string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	sess, _, _ := s.currentSession(r)
	if name == sess.Username {
		// Password sendiri diganti lewat Keamanan Akun (butuh password lama).
		writeError(w, http.StatusConflict, "self_change", "ganti password sendiri lewat Keamanan Akun")
		return
	}
	if _, exists := s.store.Get(name); !exists {
		writeError(w, http.StatusNotFound, "not_found", "user tidak ditemukan")
		return
	}
	if code, msg := s.validateNewUser(name, req.Password, req.PasswordConfirm); code != "" {
		writeErrorDetail(w, http.StatusBadRequest, code, msg, map[string]any{"field": fieldFor(code)})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	hash, ok := s.hashNew(w, r, req.Password)
	if !ok {
		return
	}
	if err := s.store.SetPassword(name, hash, req.MustChangePassword); err != nil {
		userStoreError(w, err)
		return
	}
	ended := s.sessions.DeleteUserExcept(name, "")
	s.record(r, admin.Username, "users.password_reset", true, map[string]string{"user": name, "must_change": boolText(req.MustChangePassword), "sessions_ended": itoa(ended)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ConfirmName   string `json:"confirm_name"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	sess, _, _ := s.currentSession(r)
	switch {
	case name == sess.Username:
		writeError(w, http.StatusConflict, "self_change", "tidak bisa menghapus akun sendiri")
		return
	case req.ConfirmName != name:
		writeErrorDetail(w, http.StatusBadRequest, "confirm_mismatch", "nama yang diketik ulang tidak sama", map[string]any{"field": "confirm_name"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	if err := s.store.DeleteUser(name); err != nil {
		s.record(r, admin.Username, "users.delete", false, map[string]string{"user": auditText(name), "error": err.Error()})
		userStoreError(w, err)
		return
	}
	s.sessions.DeleteUserExcept(name, "")
	s.record(r, admin.Username, "users.delete", true, map[string]string{"user": name})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// validGroupName: aturan nama sama dengan nama pengguna (nantinya juga
// menjadi grup sistem untuk izin folder), dan bukan nama yang dicadangkan.
func (s *Server) validGroupName(name string) (string, bool) {
	switch {
	case !s.groupRe.MatchString(name) || len(name) > 32:
		return "group_invalid", false
	case slices.Contains(s.cfg.Auth.ReservedGroupNames, name):
		return "group_reserved", false
	}
	return "", true
}

func (s *Server) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(strings.ToLower(req.Name))
	if code, ok := s.validGroupName(req.Name); !ok {
		writeErrorDetail(w, http.StatusBadRequest, code, "nama grup tidak sesuai aturan", map[string]any{"field": "name"})
		return
	}
	if !validDescription(req.Description) {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "keterangan tidak valid", map[string]any{"field": "description"})
		return
	}
	sess, _, _ := s.currentSession(r)
	if err := s.store.CreateGroup(auth.Group{Name: req.Name, Description: req.Description, CreatedAt: s.now().UTC()}); err != nil {
		userStoreError(w, err)
		return
	}
	s.record(r, sess.Username, "groups.create", true, map[string]string{"group": req.Name})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Description *string   `json:"description"`
		Members     *[]string `json:"members"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if (req.Description != nil && !validDescription(*req.Description)) || (req.Members != nil && len(*req.Members) > 10000) {
		writeError(w, http.StatusBadRequest, "bad_request", "isi permintaan tidak valid")
		return
	}
	name := r.PathValue("name")
	sess, _, _ := s.currentSession(r)
	if err := s.store.UpdateGroup(name, req.Description, req.Members); err != nil {
		userStoreError(w, err)
		return
	}
	detail := map[string]string{"group": name}
	if req.Members != nil {
		detail["members"] = strings.Join(*req.Members, ",")
	}
	s.record(r, sess.Username, "groups.update", true, detail)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sess, _, _ := s.currentSession(r)
	if err := s.store.DeleteGroup(name); err != nil {
		userStoreError(w, err)
		return
	}
	s.record(r, sess.Username, "groups.delete", true, map[string]string{"group": name})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleChangePassword: user mengganti password sendiri (juga langkah wajib
// setelah password dibuat atau di-reset admin). Sesi lain diakhiri.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"current_password"`
		Password        string `json:"password"`
		PasswordConfirm string `json:"password_confirm"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _, _ := s.currentSession(r)
	if code, msg := s.validateNewUser(sess.Username, req.Password, req.PasswordConfirm); code != "" {
		writeErrorDetail(w, http.StatusBadRequest, code, msg, map[string]any{"field": fieldFor(code)})
		return
	}
	if req.Password == req.CurrentPassword {
		writeErrorDetail(w, http.StatusBadRequest, "password_same", "password baru harus berbeda", map[string]any{"field": "password"})
		return
	}
	user, ok := s.reauthField(w, r, req.CurrentPassword, "current_password")
	if !ok {
		return
	}
	hash, ok := s.hashNew(w, r, req.Password)
	if !ok {
		return
	}
	if err := s.store.SetPassword(user.Username, hash, false); err != nil {
		userStoreError(w, err)
		return
	}
	ended := s.endOtherSessions(r, user.Username)
	s.record(r, user.Username, "account.password", true, map[string]string{"other_sessions_ended": itoa(ended)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "other_sessions_ended": ended})
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func itoa(n int) string { return strconv.Itoa(n) }
