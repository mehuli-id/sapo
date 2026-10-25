package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestDriveAPI(t *testing.T) {
	agent := shareAgent()
	agent.results[proto.OpHomesApply] = proto.HomesApplyResult{Failures: []string{}}
	agent.results[proto.OpHomesStatus] = []proto.HomeStatus{}
	agent.results[proto.OpHomesDelete] = map[string]bool{"ok": true}
	agent.results[proto.OpFilesList] = proto.FilesListResult{}
	h := newHarness(t, agent)
	h.runSetup()
	h.createUser("budi", "user", false)

	body := map[string]any{"pool": "tank", "quota_gb": 10}
	if rec := h.do(http.MethodPost, "/api/v1/drive/settings", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa password: %d", rec.Code)
	}
	body["admin_password"] = adminPassword
	if rec := h.do(http.MethodPost, "/api/v1/drive/settings", body); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "system_sync_failed") {
		t.Fatalf("nyalakan: %d %s", rec.Code, rec.Body)
	}
	hp, _ := agent.param(proto.OpHomesApply).(proto.HomesApplyParams)
	quota := map[string]uint64{}
	for _, u := range hp.Users {
		quota[u.Name] = u.QuotaBytes
	}
	if hp.Pool != "tank" || quota["budi"] != 10<<30 || len(quota) != 2 {
		t.Fatalf("homes.apply = %+v", hp)
	}
	if sp, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams); sp.HomesPool != "tank" {
		t.Fatalf("shares.apply tanpa folder pribadi: %+v", sp)
	}
	// homes.apply gagal (misalnya dataset induk bukan milik Sapo): [homes]
	// tidak dibagikan, dan perubahan dilaporkan belum diterapkan.
	agent.setErrs(map[string]error{proto.OpHomesApply: &proto.Error{Code: proto.ErrConflict, Message: "bukan milik Sapo Drive"}})
	if rec := h.do(http.MethodPost, "/api/v1/drive/settings", body); !strings.Contains(rec.Body.String(), "system_sync_failed") {
		t.Fatalf("galat homes.apply harus dilaporkan: %d %s", rec.Code, rec.Body)
	}
	if sp, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams); sp.HomesPool != "" {
		t.Fatalf("[homes] tidak boleh dibagikan saat homes.apply gagal: %+v", sp)
	}
	agent.setErrs(nil)
	// Kuota khusus: tanpa batas untuk budi.
	if rec := h.do(http.MethodPost, "/api/v1/drive/users/budi/quota", map[string]any{"quota_gb": -1, "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("kuota budi: %d %s", rec.Code, rec.Body)
	}
	hp, _ = agent.param(proto.OpHomesApply).(proto.HomesApplyParams)
	for _, u := range hp.Users {
		if u.Name == "budi" && u.QuotaBytes != 0 {
			t.Fatalf("kuota budi harus tanpa batas: %+v", hp)
		}
	}
	if rec := h.do(http.MethodPost, "/api/v1/drive/users/budi/quota", map[string]any{"quota_gb": 0, "admin_password": adminPassword}); rec.Code != http.StatusBadRequest {
		t.Fatalf("kuota 0: %d", rec.Code)
	}

	// File Station: "Drive saya" dibuka sebagai folder pribadi pengguna sesi.
	if rec := h.do(http.MethodGet, "/api/v1/files/shares", nil); !strings.Contains(rec.Body.String(), `"name":"@home"`) {
		t.Fatalf("daftar folder tanpa Drive saya: %s", rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/files/list?share=@home&path=/foto", nil); rec.Code != http.StatusOK {
		t.Fatalf("buka Drive saya: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpFilesList).(proto.FilesTarget); p != (proto.FilesTarget{User: "arthur", Pool: "tank", Home: true, Path: "foto"}) {
		t.Fatalf("files.list = %+v", p)
	}

	// Folder sisa pengguna lama (cici) dan folder di pool lain.
	agent.mu.Lock()
	agent.results[proto.OpHomesStatus] = []proto.HomeStatus{{Pool: "tank", User: "budi"}, {Pool: "tank", User: "cici"}}
	agent.mu.Unlock()
	rec := h.do(http.MethodPost, "/api/v1/users", map[string]any{"username": "cici", "password": userPassword, "password_confirm": userPassword,
		"role": "user", "groups": []string{}, "admin_password": adminPassword})
	if errCode(rec) != "home_exists" {
		t.Fatalf("pengguna baru bernama sama dengan folder lama: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/drive/settings", map[string]any{"pool": "lain", "admin_password": adminPassword}); errCode(rec) != "drive_pool_in_use" {
		t.Fatalf("pindah pool: %d %s", rec.Code, rec.Body)
	}
	rec = h.do(http.MethodGet, "/api/v1/drive/admin", nil)
	if !strings.Contains(rec.Body.String(), `"orphans":[{"pool":"tank","user":"cici"`) {
		t.Fatalf("admin: %s", rec.Body)
	}
	// Hapus folder: hanya pengguna yang sudah dihapus, nama diketik ulang.
	if rec := h.do(http.MethodPost, "/api/v1/drive/homes/tank/budi/delete", map[string]any{"confirm_name": "budi", "admin_password": adminPassword}); errCode(rec) != "home_user_exists" {
		t.Fatalf("hapus folder pengguna aktif: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/drive/homes/tank/cici/delete", map[string]any{"confirm_name": "cic", "admin_password": adminPassword}); errCode(rec) != "confirm_mismatch" {
		t.Fatalf("nama salah: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/drive/homes/tank/cici/delete", map[string]any{"confirm_name": "cici", "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("hapus folder sisa: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpHomesDelete).(proto.HomeDeleteParams); p != (proto.HomeDeleteParams{Pool: "tank", User: "cici"}) {
		t.Fatalf("homes.delete = %+v", p)
	}
}

func TestDriveWizardLoads(t *testing.T) {
	wz, err := LoadWizard("../../wizards", "drive-enable")
	if err != nil || wz.Action != "drive.enable" || !wz.AdminOnly {
		t.Fatalf("%+v %v", wz, err)
	}
}
