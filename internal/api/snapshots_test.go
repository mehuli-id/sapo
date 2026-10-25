package api

import (
	"net/http"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestSnapshotsAPI(t *testing.T) {
	agent := shareAgent()
	agent.results[proto.OpHomesApply] = proto.HomesApplyResult{Failures: []string{}}
	agent.results[proto.OpHomesStatus] = []proto.HomeStatus{{Pool: "tank", User: "budi"}}
	agent.results[proto.OpSnapshotsList] = proto.SnapshotsListResult{Snapshots: []proto.SnapshotInfo{{Name: "sapo-daily_GMT-2026.10.28-02.00.00", Kind: "daily"}}}
	agent.results[proto.OpSnapshotsCreate] = map[string]string{"name": "sapo-manual_GMT-2026.10.28-10.00.00"}
	agent.results[proto.OpSnapshotsDelete] = map[string]bool{"ok": true}
	h := newHarness(t, agent)
	h.runSetup()
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "keluarga", "pool": "tank", "permissions": map[string]string{}, "admin_password": adminPassword})

	pol := map[string]any{"hourly": 24, "daily": 14, "weekly": 8, "hour": 2}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/snapshot-policy", map[string]any{"policy": pol}); rec.Code != http.StatusOK {
		t.Fatalf("jadwal: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams); len(p.Shares) != 1 || p.Shares[0].Snapshots == nil || *p.Shares[0].Snapshots != (proto.SnapshotPolicy{Hourly: 24, Daily: 14, Weekly: 8, Hour: 2}) {
		t.Fatalf("shares.apply = %+v", p)
	}
	// Sinkronisasi akun gagal (izin dicabut semua): jadwal snapshot tetap.
	agent.setErrs(map[string]error{proto.OpAccountsSync: &proto.Error{Code: proto.ErrInternal, Message: "x"}})
	h.do(http.MethodPost, "/api/v1/shares/keluarga", map[string]any{"description": "baru"})
	agent.setErrs(nil)
	if p, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams); p.Shares[0].Snapshots == nil {
		t.Fatalf("jadwal snapshot hilang saat gagal-tertutup: %+v", p)
	}
	for _, bad := range []map[string]any{{"hourly": 9999}, {"hour": 24, "daily": 1}, {"daily": -1}} {
		if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/snapshot-policy", map[string]any{"policy": bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("%v harus ditolak: %d", bad, rec.Code)
		}
	}
	// Semua jenis 0 = mati.
	h.do(http.MethodPost, "/api/v1/shares/keluarga/snapshot-policy", map[string]any{"policy": map[string]any{"hour": 3}})
	if sh, _ := h.s.shares.get("keluarga"); sh.Snapshots != nil {
		t.Fatalf("jadwal kosong harus mati: %+v", sh.Snapshots)
	}
	// Folder pribadi: satu jadwal untuk semua.
	h.do(http.MethodPost, "/api/v1/drive/settings", map[string]any{"pool": "tank", "admin_password": adminPassword})
	if rec := h.do(http.MethodPost, "/api/v1/drive/snapshot-policy", map[string]any{"policy": map[string]any{"daily": 7, "hour": 1}}); rec.Code != http.StatusOK {
		t.Fatalf("jadwal Drive: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpHomesApply).(proto.HomesApplyParams); p.Snapshots == nil || p.Snapshots.Daily != 7 {
		t.Fatalf("homes.apply = %+v", p)
	}

	if rec := h.do(http.MethodGet, "/api/v1/snapshots?pool=tank&share=keluarga", nil); rec.Code != http.StatusOK {
		t.Fatalf("daftar: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/snapshots?pool=tank&share=hantu", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("folder tidak ada: %d", rec.Code)
	}
	if rec := h.do(http.MethodGet, "/api/v1/snapshots?pool=tank&user=budi", nil); rec.Code != http.StatusOK {
		t.Fatalf("daftar folder pribadi: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/snapshots", map[string]any{"pool": "tank", "share": "keluarga"}); rec.Code != http.StatusOK {
		t.Fatalf("snapshot manual: %d %s", rec.Code, rec.Body)
	}
	del := map[string]any{"pool": "tank", "share": "keluarga", "name": "sapo-daily_GMT-2026.10.28-02.00.00"}
	if rec := h.do(http.MethodPost, "/api/v1/snapshots/delete", del); rec.Code != http.StatusUnauthorized {
		t.Fatalf("hapus tanpa password: %d", rec.Code)
	}
	del["admin_password"] = adminPassword
	if rec := h.do(http.MethodPost, "/api/v1/snapshots/delete", del); rec.Code != http.StatusOK {
		t.Fatalf("hapus: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpSnapshotsDelete).(proto.SnapshotParams); p.Name != "sapo-daily_GMT-2026.10.28-02.00.00" || p.Share != "keluarga" {
		t.Fatalf("snapshots.delete = %+v", p)
	}
	// Pengguna biasa tidak bisa mengatur atau melihat snapshot di sini.
	h.createUser("ani", "user", false)
	h.loginAs("ani", userPassword)
	if rec := h.do(http.MethodGet, "/api/v1/snapshots?pool=tank&user=budi", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("pengguna biasa: %d", rec.Code)
	}
}
