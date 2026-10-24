package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/auth"
	"github.com/mehuli-id/sapo/internal/proto"
)

const userPassword = "sawah hijau di lembah"

// createUser membuat user lewat API sebagai admin yang sedang login.
func (h *harness) createUser(name, role string, mustChange bool, groups ...string) {
	h.t.Helper()
	if groups == nil {
		groups = []string{}
	}
	rec := h.do(http.MethodPost, "/api/v1/users", map[string]any{
		"username": name, "password": userPassword, "password_confirm": userPassword, "role": role,
		"groups": groups, "must_change_password": mustChange, "admin_password": adminPassword,
	})
	if rec.Code != http.StatusOK {
		h.t.Fatalf("buat user %s: %d %s", name, rec.Code, rec.Body)
	}
}

// loginAs masuk sebagai user lain dan mengembalikan cookie admin semula.
func (h *harness) loginAs(name, password string) *http.Cookie {
	h.t.Helper()
	admin := h.cookie
	h.cookie = nil
	rec := h.do(http.MethodPost, "/api/v1/auth/login", map[string]string{"username": name, "password": password})
	if rec.Code != http.StatusOK {
		h.t.Fatalf("login %s: %d %s", name, rec.Code, rec.Body)
	}
	return admin
}

func pending(h *harness) []string {
	var st struct {
		User struct {
			Role    string   `json:"role"`
			Pending []string `json:"pending"`
		} `json:"user"`
	}
	_ = json.Unmarshal(h.do(http.MethodGet, "/api/v1/auth/status", nil).Body.Bytes(), &st)
	return st.User.Pending
}

func TestUserLifecycle(t *testing.T) {
	h := newHarness(t, &fakeAgent{results: map[string]any{proto.OpSystemInfo: proto.SystemInfo{Hostname: "nas"}}})
	h.runSetup()

	// Membuat user butuh password admin.
	rec := h.do(http.MethodPost, "/api/v1/users", map[string]any{
		"username": "budi", "password": userPassword, "password_confirm": userPassword, "role": "user",
		"groups": []string{}, "admin_password": "bukan password admin",
	})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"field":"admin_password"`) {
		t.Fatalf("tanpa password admin: %d %s", rec.Code, rec.Body)
	}
	h.createUser("budi", "user", true)
	if rec := h.do(http.MethodPost, "/api/v1/users", map[string]any{"username": "Budi", "password": userPassword, "password_confirm": userPassword, "role": "user", "admin_password": adminPassword}); rec.Code != http.StatusConflict || errCode(rec) != "username_taken" {
		t.Fatalf("nama ganda: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/users", map[string]any{"username": "games", "password": userPassword, "password_confirm": userPassword, "role": "user", "admin_password": adminPassword}); errCode(rec) != "username_reserved" || rec.Code != http.StatusBadRequest {
		t.Fatalf("nama sistem: %d %s", rec.Code, rec.Body)
	}

	// Login pertama: wajib ganti password; panel lain ditolak sampai selesai.
	admin := h.loginAs("budi", userPassword)
	if p := pending(h); len(p) != 1 || p[0] != "password" {
		t.Fatalf("langkah wajib = %v", p)
	}
	if rec := h.do(http.MethodGet, "/api/v1/system/info", nil); rec.Code != http.StatusForbidden || errCode(rec) != "account_setup_required" {
		t.Fatalf("sebelum ganti password: %d %s", rec.Code, rec.Body)
	}
	newPass := "padi menguning di sawah"
	if rec := h.do(http.MethodPost, "/api/v1/account/password", map[string]string{"current_password": userPassword, "password": userPassword, "password_confirm": userPassword}); errCode(rec) != "password_same" {
		t.Fatalf("password sama: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/account/password", map[string]string{"current_password": userPassword, "password": newPass, "password_confirm": newPass}); rec.Code != http.StatusOK {
		t.Fatalf("ganti password: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/system/info", nil); rec.Code != http.StatusOK {
		t.Fatalf("setelah ganti password: %d", rec.Code)
	}
	// User biasa tidak bisa mengelola user atau melihat storage.
	for _, p := range []string{"/api/v1/users", "/api/v1/groups", "/api/v1/storage/disks", "/api/v1/storage/pools", "/api/v1/features"} {
		if rec := h.do(http.MethodGet, p, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s untuk user biasa: %d", p, rec.Code)
		}
	}
	budi := h.cookie

	// Admin menonaktifkan budi: sesinya berakhir dan login ditolak.
	h.cookie = admin
	if rec := h.do(http.MethodPost, "/api/v1/users/budi", map[string]any{"disabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("nonaktifkan: %d %s", rec.Code, rec.Body)
	}
	h.cookie = budi
	if rec := h.do(http.MethodGet, "/api/v1/system/info", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sesi user nonaktif harus berakhir: %d", rec.Code)
	}
	h.cookie = nil
	if rec := h.do(http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "budi", "password": newPass}); rec.Code != http.StatusForbidden || errCode(rec) != "account_disabled" {
		t.Fatalf("login nonaktif: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/auth/login", map[string]string{"username": "budi", "password": "salah sekali ini"}); errCode(rec) != "bad_credentials" {
		t.Fatalf("password salah tidak boleh membocorkan status akun: %s", rec.Body)
	}

	// Reset password oleh admin: wajib ganti lagi.
	h.cookie = admin
	// Mengaktifkan lagi memberi akses: butuh password admin.
	if rec := h.do(http.MethodPost, "/api/v1/users/budi", map[string]any{"disabled": false}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("aktifkan tanpa password: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/users/budi", map[string]any{"disabled": false, "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("aktifkan: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/users/budi/password", map[string]any{"password": userPassword, "password_confirm": userPassword, "must_change_password": true, "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("reset password: %d %s", rec.Code, rec.Body)
	}
	h.loginAs("budi", userPassword)
	if p := pending(h); len(p) != 1 || p[0] != "password" {
		t.Fatalf("setelah reset = %v", p)
	}

	// Hapus: nama diketik ulang + password admin; akun sendiri tidak bisa.
	h.cookie = admin
	if rec := h.do(http.MethodPost, "/api/v1/users/arthur/delete", map[string]string{"confirm_name": "arthur", "admin_password": adminPassword}); errCode(rec) != "self_change" {
		t.Fatalf("hapus diri sendiri: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/users/budi/delete", map[string]string{"confirm_name": "bud", "admin_password": adminPassword}); errCode(rec) != "confirm_mismatch" {
		t.Fatalf("konfirmasi salah: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/users/budi/delete", map[string]string{"confirm_name": "budi", "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("hapus: %d %s", rec.Code, rec.Body)
	}
	b, _ := os.ReadFile(filepath.Join(h.data, "audit.log"))
	for _, a := range []string{"users.create", "users.update", "users.password_reset", "users.delete", "account.password"} {
		if !strings.Contains(string(b), `"action":"`+a+`"`) {
			t.Errorf("log audit tanpa %s", a)
		}
	}
	if strings.Contains(string(b), userPassword) || strings.Contains(string(b), adminPassword) {
		t.Fatal("password masuk log audit")
	}
}

func TestNewAdminMustEnrollTOTPAndRoleChanges(t *testing.T) {
	h := newHarness(t, &fakeAgent{results: map[string]any{proto.OpSystemInfo: proto.SystemInfo{}}})
	h.runSetup()
	h.createUser("sari", "admin", false)
	admin := h.loginAs("sari", userPassword)
	// Admin baru selalu wajib mengganti password dari pembuatnya, lalu 2FA.
	if p := pending(h); strings.Join(p, ",") != "password,totp" {
		t.Fatalf("admin baru: %v", p)
	}
	sariPass := "kebun teh di lereng gunung"
	if rec := h.do(http.MethodPost, "/api/v1/account/password", map[string]string{"current_password": userPassword, "password": sariPass, "password_confirm": sariPass}); rec.Code != http.StatusOK {
		t.Fatalf("ganti password admin baru: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/users", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("admin tanpa 2FA harus ditolak dulu: %d", rec.Code)
	}
	// Pendaftaran 2FA tetap bisa dilakukan, lalu panel terbuka.
	rec := h.do(http.MethodPost, "/api/v1/account/totp/begin", map[string]string{"password": sariPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("mulai 2FA: %d %s", rec.Code, rec.Body)
	}
	var en setupTOTPResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &en)
	code, _ := auth.TOTPCodeAt(en.Secret, h.now)
	if rec := h.do(http.MethodPost, "/api/v1/account/totp/confirm", map[string]string{"token": en.Token, "code": code}); rec.Code != http.StatusOK {
		t.Fatalf("konfirmasi 2FA: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/account/recovery-codes", map[string]string{"password": sariPass}); rec.Code != http.StatusOK {
		t.Fatalf("kode cadangan: %d %s", rec.Code, rec.Body)
	}
	if p := pending(h); len(p) != 0 {
		t.Fatalf("setelah 2FA masih ada langkah wajib: %v", p)
	}
	if rec := h.do(http.MethodGet, "/api/v1/users", nil); rec.Code != http.StatusOK {
		t.Fatalf("admin baru setelah 2FA: %d", rec.Code)
	}
	sari := h.cookie

	// Menurunkan peran butuh password admin dan mengakhiri sesi.
	h.cookie = admin
	if rec := h.do(http.MethodPost, "/api/v1/users/sari", map[string]any{"role": "user"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ubah peran tanpa password: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/users/sari", map[string]any{"role": "user", "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("ubah peran: %d %s", rec.Code, rec.Body)
	}
	h.cookie = sari
	if rec := h.do(http.MethodGet, "/api/v1/auth/status", nil); strings.Contains(rec.Body.String(), `"authenticated":true`) {
		t.Fatal("sesi harus berakhir setelah peran berubah")
	}
	// Admin tidak bisa menurunkan atau menonaktifkan dirinya sendiri.
	h.cookie = admin
	if rec := h.do(http.MethodPost, "/api/v1/users/arthur", map[string]any{"disabled": true}); errCode(rec) != "self_change" {
		t.Fatalf("nonaktifkan diri sendiri: %d %s", rec.Code, rec.Body)
	}
}

func TestGroups(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	h.runSetup()
	if rec := h.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": "Keluarga", "description": "Satu rumah"}); rec.Code != http.StatusOK {
		t.Fatalf("buat grup: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"users", "sudo", "a", "-x", "kel uarga"} {
		if rec := h.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("nama grup %q harus ditolak: %d", bad, rec.Code)
		}
	}
	h.createUser("budi", "user", false, "keluarga")
	if rec := h.do(http.MethodPost, "/api/v1/users", map[string]any{"username": "ani", "password": userPassword, "password_confirm": userPassword, "role": "user", "groups": []string{"tidak-ada"}, "admin_password": adminPassword}); errCode(rec) != "group_not_found" {
		t.Fatalf("grup tidak ada: %d %s", rec.Code, rec.Body)
	}
	h.createUser("ani", "user", false)
	if rec := h.do(http.MethodPost, "/api/v1/groups/keluarga", map[string]any{"members": []string{"ani", "arthur"}}); rec.Code != http.StatusOK {
		t.Fatalf("anggota: %d %s", rec.Code, rec.Body)
	}
	var groups []groupView
	_ = json.Unmarshal(h.do(http.MethodGet, "/api/v1/groups", nil).Body.Bytes(), &groups)
	if len(groups) != 1 || strings.Join(groups[0].Members, ",") != "ani,arthur" {
		t.Fatalf("grup = %+v", groups)
	}
	if rec := h.do(http.MethodPost, "/api/v1/groups/keluarga/delete", nil); rec.Code != http.StatusOK {
		t.Fatalf("hapus grup: %d", rec.Code)
	}
	var users []userView
	_ = json.Unmarshal(h.do(http.MethodGet, "/api/v1/users", nil).Body.Bytes(), &users)
	for _, u := range users {
		if len(u.Groups) != 0 {
			t.Fatalf("keanggotaan grup terhapus masih ada: %+v", u)
		}
	}
}

func TestUserWizardLoads(t *testing.T) {
	wz, err := LoadWizard("../../wizards", "user-create")
	if err != nil || wz.Action != "users.create" {
		t.Fatalf("%+v %v", wz, err)
	}
}

func TestUserGroupNamespaceAndWizardAccess(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	h.runSetup()
	h.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": "keuangan"})
	// Nama grup tidak boleh dipakai pengguna, nama grup sistem juga tidak.
	for _, name := range []string{"keuangan", "sudo", "disk"} {
		rec := h.do(http.MethodPost, "/api/v1/users", map[string]any{"username": name, "password": userPassword, "password_confirm": userPassword, "role": "user", "admin_password": adminPassword})
		if rec.Code == http.StatusOK {
			t.Errorf("pengguna %q harus ditolak", name)
		}
	}
	h.createUser("budi", "user", false)
	for _, name := range []string{"budi", "games"} {
		if rec := h.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": name}); rec.Code == http.StatusOK {
			t.Errorf("grup %q harus ditolak", name)
		}
	}
	if rec := h.do(http.MethodPost, "/api/v1/users/budi", map[string]any{"description": "Budi\u202emoc.knab"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("karakter pembalik arah harus ditolak: %d", rec.Code)
	}
	// Wizard admin hanya untuk admin; pengguna biasa ditolak.
	if rec := h.do(http.MethodGet, "/api/v1/wizards/user-create", nil); rec.Code != http.StatusOK {
		t.Fatalf("wizard untuk admin: %d", rec.Code)
	}
	h.loginAs("budi", userPassword)
	for _, id := range []string{"user-create", "pool-create"} {
		if rec := h.do(http.MethodGet, "/api/v1/wizards/"+id, nil); rec.Code != http.StatusForbidden {
			t.Errorf("wizard %s untuk pengguna biasa: %d", id, rec.Code)
		}
	}
}
