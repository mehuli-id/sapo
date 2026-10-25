package api

import (
	"net/http"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestBackupsAPI(t *testing.T) {
	agent := shareAgent()
	agent.results[proto.OpBackupsSet] = map[string]bool{"ok": true}
	agent.results[proto.OpBackupsRun] = map[string]bool{"ok": true}
	agent.results[proto.OpBackupsDelete] = map[string]bool{"ok": true}
	agent.results[proto.OpBackupsRestore] = map[string]bool{"ok": true}
	agent.results[proto.OpBackupsList] = proto.BackupsListResult{Jobs: []proto.BackupStatus{}, Restores: []proto.BackupRestore{}}
	h := newHarness(t, agent)
	h.runSetup()
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "keluarga", "pool": "tank", "permissions": map[string]string{"user:arthur": "write"}, "admin_password": adminPassword})

	job := map[string]any{"share": "keluarga", "dest_pool": "usb", "hour": 2, "keep": 7, "enabled": true}
	if rec := h.do(http.MethodPost, "/api/v1/backups", job); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusBadRequest {
		t.Fatalf("tanpa password: %d %s", rec.Code, rec.Body)
	}
	job["admin_password"] = adminPassword
	if rec := h.do(http.MethodPost, "/api/v1/backups", job); rec.Code != http.StatusOK {
		t.Fatalf("buat tugas: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpBackupsSet).(proto.BackupJob); p.ID != "keluarga-usb" || p.SrcPool != "tank" || p.DestPool != "usb" || p.Keep != 7 {
		t.Fatalf("backups.set = %+v", p)
	}
	job["dest_pool"] = "tank"
	if rec := h.do(http.MethodPost, "/api/v1/backups", job); errCode(rec) != "backup_same_pool" {
		t.Fatalf("pool sama: %d %s", rec.Code, rec.Body)
	}
	// Hapus beserta salinan: ketik ulang ID.
	if rec := h.do(http.MethodPost, "/api/v1/backups/keluarga-usb/delete", map[string]any{"delete_data": true, "confirm_name": "salah", "admin_password": adminPassword}); errCode(rec) != "confirm_mismatch" {
		t.Fatalf("konfirmasi salah: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/backups/keluarga-usb/delete", map[string]any{"delete_data": true, "confirm_name": "keluarga-usb", "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("hapus: %d %s", rec.Code, rec.Body)
	}
	// Pulihkan: nama folder yang sudah ada ditolak.
	if rec := h.do(http.MethodPost, "/api/v1/backups/keluarga-usb/restore", map[string]any{"copy": "x", "name": "keluarga", "admin_password": adminPassword}); errCode(rec) != "share_taken" {
		t.Fatalf("nama terpakai: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/backups/keluarga-usb/restore", map[string]any{"copy": "x", "name": "pulih", "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("pulihkan: %d %s", rec.Code, rec.Body)
	}
	// Selesai tetapi tidak ada lagi di pool: tidak didaftarkan.
	agent.mu.Lock()
	agent.results[proto.OpBackupsList] = proto.BackupsListResult{
		Jobs:     []proto.BackupStatus{{BackupJob: proto.BackupJob{ID: "keluarga-usb", SrcPool: "tank", Share: "keluarga", DestPool: "usb"}}},
		Restores: []proto.BackupRestore{{JobID: "keluarga-usb", Pool: "tank", Name: "pulih", OK: true, Present: false}},
	}
	agent.mu.Unlock()
	h.do(http.MethodGet, "/api/v1/backups", nil)
	if _, ok := h.s.shares.get("pulih"); ok {
		t.Fatal("folder pulihan yang tidak ada tidak boleh didaftarkan")
	}
	// Ada: didaftarkan dengan izin folder asal.
	agent.mu.Lock()
	agent.results[proto.OpBackupsList] = proto.BackupsListResult{
		Jobs:     []proto.BackupStatus{{BackupJob: proto.BackupJob{ID: "keluarga-usb", SrcPool: "tank", Share: "keluarga", DestPool: "usb"}}},
		Restores: []proto.BackupRestore{{JobID: "keluarga-usb", Pool: "tank", Name: "pulih", OK: true, Present: true}},
	}
	agent.mu.Unlock()
	h.do(http.MethodGet, "/api/v1/backups", nil)
	sh, ok := h.s.shares.get("pulih")
	if !ok || sh.Pool != "tank" || sh.Permissions["user:arthur"] != "write" {
		t.Fatalf("folder pulihan = %+v %v", sh, ok)
	}
}

func TestBackupID(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"keluarga", "usb"}:  "keluarga-usb",
		{"Foto.2026", "cad"}: "foto-2026-cad",
		{"9abc", "usb"}:      "abc-usb",
	} {
		if got := backupID(in[0], in[1]); got != want {
			t.Errorf("backupID(%v) = %q, mau %q", in, got, want)
		}
	}
}

func TestBackupWizardLoads(t *testing.T) {
	wz, err := LoadWizard("../../wizards", "backup-create")
	if err != nil || wz.Action != "backups.create" || !wz.AdminOnly {
		t.Fatalf("wizard = %+v, %v", wz, err)
	}
}
