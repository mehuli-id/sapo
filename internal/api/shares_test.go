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
	agent.setErrs(map[string]error{proto.OpSharesDelete: &proto.Error{Code: proto.ErrPoolBusy, Message: "busy"}})
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/delete", map[string]string{"confirm_name": "keluarga", "admin_password": adminPassword}); rec.Code != http.StatusConflict {
		t.Fatalf("hapus saat sibuk: %d %s", rec.Code, rec.Body)
	}
	if _, ok := h.s.shares.get("keluarga"); !ok {
		t.Fatal("folder yang gagal dihapus harus tetap ada")
	}
	agent.setErrs(nil)
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
	agent.forget(proto.OpSMBPassword)
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
	agent.setErrs(map[string]error{proto.OpAccountsSync: &proto.Error{Code: proto.ErrConflict, Message: "catatan rusak"}})
	if _, err := h.s.syncSystem(context.Background()); err == nil {
		t.Fatal("sinkronisasi harus gagal")
	}
	apply, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams)
	if len(apply.Shares) != 1 || len(apply.Shares[0].Read)+len(apply.Shares[0].Write) != 0 {
		t.Fatalf("shares.apply = %+v", apply)
	}
}

func TestShareNFS(t *testing.T) {
	agent := shareAgent()
	h := newHarness(t, agent)
	h.runSetup()
	h.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": "ortu"})
	h.createUser("ani", "user", false, "ortu")
	h.createUser("budi", "user", false)

	body := map[string]any{
		"name": "keluarga", "pool": "tank", "admin_password": adminPassword,
		"permissions": map[string]string{"group:ortu": "write", "user:budi": "read"},
		// Alamat tunggal menjadi /32; jaringan disamakan ke bentuk kanonis.
		"nfs": map[string]any{"user": "ani", "clients": []string{" 192.168.1.20 ", "10.1.2.3/16", "10.1.0.0/16"}},
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares", body); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"nfs_access":"write"`) {
		t.Fatalf("buat folder: %d %s", rec.Code, rec.Body)
	}
	// Izin ani lewat grup ortu: diekspor baca & tulis.
	apply, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams)
	if n := apply.Shares[0].NFS; n == nil || n.User != "ani" || n.ReadOnly || strings.Join(n.Clients, ",") != "192.168.1.20/32,10.1.0.0/16" {
		t.Fatalf("shares.apply = %+v", apply.Shares[0].NFS)
	}

	for name, nfs := range map[string]any{
		"semua orang":    map[string]any{"user": "ani", "clients": []string{"0.0.0.0/0"}},
		"terlalu luas":   map[string]any{"user": "ani", "clients": []string{"10.0.0.0/7"}},
		"nama host":      map[string]any{"user": "ani", "clients": []string{"pc-ani.lan"}},
		"alamat publik":  map[string]any{"user": "ani", "clients": []string{"8.8.8.8"}},
		"tanpa klien":    map[string]any{"user": "ani", "clients": []string{}},
		"pengguna asing": map[string]any{"user": "hantu", "clients": []string{"10.0.0.0/8"}},
	} {
		rec := h.do(http.MethodPost, "/api/v1/shares/keluarga", map[string]any{"nfs": nfs, "admin_password": adminPassword})
		if errCode(rec) != "nfs_invalid" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// Mengubah ekspor NFS butuh password admin.
	nfsBudi := map[string]any{"user": "budi", "clients": []string{"10.0.0.0/8"}}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga", map[string]any{"nfs": nfsBudi}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ubah NFS tanpa password: %d", rec.Code)
	}
	// budi hanya punya izin baca: diekspor baca saja.
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga", map[string]any{"nfs": nfsBudi, "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("ubah NFS: %d %s", rec.Code, rec.Body)
	}
	apply, _ = agent.param(proto.OpSharesApply).(proto.SharesApplyParams)
	if n := apply.Shares[0].NFS; n == nil || n.User != "budi" || !n.ReadOnly {
		t.Fatalf("shares.apply = %+v", n)
	}
	// Pengguna tanpa izin di folder: tidak diekspor sama sekali.
	h.do(http.MethodPost, "/api/v1/shares/keluarga", map[string]any{"permissions": map[string]string{"group:ortu": "write"}, "admin_password": adminPassword})
	if apply, _ = agent.param(proto.OpSharesApply).(proto.SharesApplyParams); apply.Shares[0].NFS != nil {
		t.Fatalf("budi tanpa izin tidak boleh diekspor: %+v", apply.Shares[0].NFS)
	}
	// Mengubah keterangan saja tidak mengubah ekspor NFS.
	h.do(http.MethodPost, "/api/v1/shares/keluarga", map[string]any{"description": "baru"})
	if sh, _ := h.s.shares.get("keluarga"); sh.NFS == nil || sh.NFS.User != "budi" {
		t.Fatalf("NFS hilang: %+v", sh.NFS)
	}
	// Menghapus pengguna NFS mematikan ekspornya.
	h.do(http.MethodPost, "/api/v1/users/budi/delete", map[string]string{"confirm_name": "budi", "admin_password": adminPassword})
	if sh, _ := h.s.shares.get("keluarga"); sh.NFS != nil {
		t.Fatalf("NFS pengguna terhapus harus mati: %+v", sh.NFS)
	}
	// null mematikan ekspor.
	h.do(http.MethodPost, "/api/v1/shares/keluarga", map[string]any{"nfs": map[string]any{"user": "ani", "clients": []string{"10.0.0.0/8"}}, "admin_password": adminPassword})
	h.do(http.MethodPost, "/api/v1/shares/keluarga", map[string]any{"nfs": nil, "admin_password": adminPassword})
	if sh, _ := h.s.shares.get("keluarga"); sh.NFS != nil {
		t.Fatalf("NFS harus mati: %+v", sh.NFS)
	}
}

func TestNetworkAPI(t *testing.T) {
	agent := shareAgent()
	agent.results[proto.OpNetworkApply] = proto.NetworkApplyResult{Token: strings.Repeat("a", 64)}
	agent.results[proto.OpNetworkConfirm] = map[string]bool{"ok": true}
	h := newHarness(t, agent)
	h.runSetup()
	body := map[string]any{"config": map[string]any{"hostname": "nas", "interfaces": []any{}, "dns": []string{}}}
	if rec := h.do(http.MethodPost, "/api/v1/network", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa password admin: %d", rec.Code)
	}
	body["admin_password"] = adminPassword
	if rec := h.do(http.MethodPost, "/api/v1/network", body); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), strings.Repeat("a", 64)) {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpNetworkApply).(proto.NetworkApplyParams); p.Config.Hostname != "nas" {
		t.Fatalf("network.apply = %+v", p)
	}
	// Konfirmasi tanpa login (alamat baru), cukup dengan token.
	h.cookie = nil
	if rec := h.do(http.MethodPost, "/api/v1/network/confirm", map[string]string{"token": "pendek"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("token pendek: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/network/confirm", map[string]string{"token": strings.Repeat("a", 64)}); rec.Code != http.StatusOK {
		t.Fatalf("konfirmasi: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/network", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status jaringan tanpa login: %d", rec.Code)
	}
}
