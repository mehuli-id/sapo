package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/auth"
	"github.com/mehuli-id/sapo/internal/proto"
)

func shareAgent() *fakeAgent {
	return &fakeAgent{results: map[string]any{
		proto.OpAccountsSync: proto.AccountsSyncResult{Conflicts: []string{"user:bentrok"}},
		proto.OpSharesApply:  proto.SharesApplyResult{SMB: "applied", Shares: []proto.ShareState{{Name: "keluarga", Pool: "tank", Path: "/srv/sapo/tank/keluarga"}}},
		proto.OpSharesUsage:  map[string]proto.ShareUsage{"tank/keluarga": {UsedBytes: 4096, AvailableBytes: 1 << 30}},
		proto.OpSharesDelete: map[string]bool{"ok": true},
		proto.OpSMBPassword:  map[string]bool{"ok": true},
	}}
}

func (f *fakeAgent) param(op string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.params[op]
}

func TestShareLifecycle(t *testing.T) {
	agent := shareAgent()
	h := newHarness(t, agent)
	h.runSetup()
	h.createUser("budi", "user", false)
	h.createUser("bentrok", "user", false)
	h.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": "keluarga-besar"})

	// Membuat user: akun sistem disinkronkan lalu password Samba disamakan.
	if p, _ := agent.param(proto.OpSMBPassword).(proto.SMBPasswordParams); p.Name != "bentrok" || p.Password != userPassword {
		t.Fatalf("password Samba = %+v", agent.param(proto.OpSMBPassword))
	}

	body := map[string]any{
		"name": "Keluarga", "pool": "tank", "description": "Foto keluarga",
		"permissions": map[string]string{"user:budi": "write", "group:keluarga-besar": "read", "user:bentrok": "write"},
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa password admin: %d", rec.Code)
	}
	body["admin_password"] = adminPassword
	rec := h.do(http.MethodPost, "/api/v1/shares", body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"path":"/srv/sapo/tank/keluarga"`) {
		t.Fatalf("buat folder: %d %s", rec.Code, rec.Body)
	}
	// Ke sapod: izin hanya untuk akun yang tidak bentrok dengan akun sistem.
	apply, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams)
	if len(apply.Shares) != 1 || strings.Join(apply.Shares[0].Write, ",") != "user:budi" || strings.Join(apply.Shares[0].Read, ",") != "group:keluarga-besar" {
		t.Fatalf("shares.apply = %+v", apply)
	}
	acc, _ := agent.param(proto.OpAccountsSync).(proto.AccountsSyncParams)
	if len(acc.Users) != 3 || len(acc.Groups) != 1 {
		t.Fatalf("accounts.sync = %+v", acc)
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares", body); rec.Code != http.StatusConflict {
		t.Fatalf("nama ganda: %d", rec.Code)
	}
	for _, bad := range []map[string]any{
		{"name": "homes", "pool": "tank", "admin_password": adminPassword},
		{"name": "data", "pool": "tank", "description": "a\nb", "admin_password": adminPassword},
		{"name": "data", "pool": "tank", "permissions": map[string]string{"user:hantu": "write"}, "admin_password": adminPassword},
		{"name": "data", "pool": "tank", "permissions": map[string]string{"user:budi": "admin"}, "admin_password": adminPassword},
	} {
		if rec := h.do(http.MethodPost, "/api/v1/shares", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%v harus ditolak: %d", bad, rec.Code)
		}
	}

	var list []shareView
	_ = json.Unmarshal(h.do(http.MethodGet, "/api/v1/shares", nil).Body.Bytes(), &list)
	if len(list) != 1 || list[0].UsedBytes != 4096 {
		t.Fatalf("daftar = %+v", list)
	}

	// Mengubah izin butuh password admin; menghapus user membuang izinnya.
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga", map[string]any{"permissions": map[string]string{"user:budi": "read"}}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ubah izin tanpa password: %d", rec.Code)
	}
	h.do(http.MethodPost, "/api/v1/users/budi/delete", map[string]string{"confirm_name": "budi", "admin_password": adminPassword})
	if sh, _ := h.s.shares.get("keluarga"); sh.Permissions["user:budi"] != "" {
		t.Fatalf("izin user terhapus masih ada: %v", sh.Permissions)
	}

	// Hapus: gagal di sapod = folder tetap ada di daftar.
	agent.mu.Lock()
	agent.errs = map[string]error{proto.OpSharesDelete: &proto.Error{Code: proto.ErrPoolBusy, Message: "busy"}}
	agent.mu.Unlock()
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/delete", map[string]string{"confirm_name": "keluarga", "admin_password": adminPassword}); rec.Code != http.StatusConflict {
		t.Fatalf("hapus saat sibuk: %d %s", rec.Code, rec.Body)
	}
	if _, ok := h.s.shares.get("keluarga"); !ok {
		t.Fatal("folder yang gagal dihapus harus tetap ada")
	}
	agent.mu.Lock()
	agent.errs = nil
	agent.mu.Unlock()
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/delete", map[string]string{"confirm_name": "kel", "admin_password": adminPassword}); errCode(rec) != "confirm_mismatch" {
		t.Fatalf("konfirmasi salah: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/delete", map[string]string{"confirm_name": "keluarga", "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("hapus: %d %s", rec.Code, rec.Body)
	}
	if _, ok := h.s.shares.get("keluarga"); ok {
		t.Fatal("folder harus hilang dari daftar")
	}
	b, _ := os.ReadFile(filepath.Join(h.data, "audit.log"))
	for _, a := range []string{"shares.create", "shares.delete.request", `"shares.delete","ip"`} {
		if !strings.Contains(string(b), a) {
			t.Errorf("log audit tanpa %s", a)
		}
	}
	// Pengguna biasa tidak bisa melihat atau mengubah folder.
	h.createUser("ani", "user", false)
	h.loginAs("ani", userPassword)
	if rec := h.do(http.MethodGet, "/api/v1/shares", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("pengguna biasa: %d", rec.Code)
	}
}

func TestSMBPasswordSkipsStaleOrDisabled(t *testing.T) {
	agent := shareAgent()
	h := newHarness(t, agent)
	h.runSetup()
	h.createUser("budi", "user", false)
	agent.mu.Lock()
	agent.params[proto.OpSMBPassword] = nil
	agent.mu.Unlock()
	// Password lama dari login yang tertunda: hash sudah berbeda, dilewati.
	h.s.setSMBPassword(context.Background(), "budi", "password lama sekali", "hash-lama")
	if agent.param(proto.OpSMBPassword) != nil {
		t.Fatal("password lama tidak boleh dipasang lagi")
	}
	// Akun nonaktif tidak diberi password Samba (bisa mengaktifkannya lagi).
	disabled := true
	_ = h.s.store.UpdateUser("budi", auth.UserChange{Disabled: &disabled})
	h.s.setSMBPassword(context.Background(), "budi", userPassword, "")
	if agent.param(proto.OpSMBPassword) != nil {
		t.Fatal("akun nonaktif tidak boleh diberi password Samba")
	}
	// Karakter kendali dan password terlalu panjang ditolak panel.
	for _, pw := range []string{"tab\tdi tengah ini", strings.Repeat("x", 128)} {
		rec := h.do(http.MethodPost, "/api/v1/users", map[string]any{"username": "cici", "password": pw, "password_confirm": pw, "role": "user", "admin_password": adminPassword})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("password %q: %d", pw[:5], rec.Code)
		}
	}
}

func TestSyncFailsClosedWhenAccountsFail(t *testing.T) {
	agent := shareAgent()
	h := newHarness(t, agent)
	h.runSetup()
	h.createUser("budi", "user", false)
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "data", "pool": "tank", "permissions": map[string]string{"user:budi": "write"}, "admin_password": adminPassword})
	// Akun tidak bisa disinkronkan (misalnya catatan akun rusak): semua
	// folder dikirim tanpa izin.
	agent.mu.Lock()
	agent.errs = map[string]error{proto.OpAccountsSync: &proto.Error{Code: proto.ErrConflict, Message: "catatan rusak"}}
	agent.mu.Unlock()
	if _, err := h.s.syncSystem(context.Background()); err == nil {
		t.Fatal("sinkronisasi harus gagal")
	}
	apply, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams)
	if len(apply.Shares) != 1 || len(apply.Shares[0].Read)+len(apply.Shares[0].Write) != 0 {
		t.Fatalf("shares.apply = %+v", apply)
	}
}
