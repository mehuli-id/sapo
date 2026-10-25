package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/proto"
)

func TestUpdateCheckDue(t *testing.T) {
	loc := time.FixedZone("WITA", 8*3600)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 28, h, m, 0, 0, loc) }
	f := updateCheckFile{}
	if updateCheckDue(f, 4, at(3, 59), loc) != true {
		// Belum pernah: kemarin pukul 4 sudah lewat, jadi langsung diperiksa.
		t.Fatal("belum pernah diperiksa harus jalan")
	}
	f.Last = &proto.UpdatesCheckResult{CheckedAt: at(4, 5).UTC()}
	if updateCheckDue(f, 4, at(10, 0), loc) {
		t.Fatal("sudah diperiksa hari ini")
	}
	if !updateCheckDue(f, 4, at(4, 1).AddDate(0, 0, 1), loc) {
		t.Fatal("besok pukul 4 harus jalan")
	}
	f.LastAttempt = at(3, 59).AddDate(0, 0, 1).UTC()
	if !updateCheckDue(f, 4, at(4, 30).AddDate(0, 0, 1), loc) {
		// Percobaan terakhir sebelum jadwal hari itu: tidak ditahan.
		t.Fatal("percobaan lama tidak menahan jadwal baru")
	}
	f.LastAttempt = at(4, 10).AddDate(0, 0, 1).UTC()
	if updateCheckDue(f, 4, at(4, 30).AddDate(0, 0, 1), loc) {
		t.Fatal("gagal 20 menit lalu: tunggu sejam")
	}
	f.Settings.Off = true
	if updateCheckDue(f, 4, at(23, 0).AddDate(0, 0, 2), loc) {
		t.Fatal("mati")
	}
}

func TestUpdateCheckNotify(t *testing.T) {
	agent := shareAgent()
	agent.results[proto.OpUpdatesCheck] = proto.UpdatesCheckResult{CheckedAt: time.Now(), Packages: []proto.UpdatePackage{{Name: "curl", To: "8"}, {Name: "sapo", To: "0.2"}}}
	agent.results[proto.OpStoragePools] = proto.PoolsResult{Available: true}
	agent.results[proto.OpStorageDisks] = []proto.Disk{}
	agent.results[proto.OpBackupsList] = proto.BackupsListResult{}
	h := newHarness(t, agent)
	shipped, err := config.Load("../../config/sapo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	h.s.cfg.API.Notify.Messages = shipped.API.Notify.Messages
	h.runSetup()
	sent := &sentMail{}
	h.s.notify.send = sent.send
	h.do(http.MethodPost, "/api/v1/notify", map[string]any{"enabled": true, "host": "127.0.0.1", "port": 2525, "security": "none", "from": "sapo@uji.local", "to": []string{"admin@uji.local"}, "admin_password": adminPassword})

	if rec := h.do(http.MethodPost, "/api/v1/updates/schedule", map[string]any{"off": false, "hour": 24}); rec.Code != http.StatusBadRequest {
		t.Fatalf("jam 24: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/updates/schedule", map[string]any{"off": false, "hour": 2}); rec.Code != http.StatusOK {
		t.Fatalf("jadwal: %d %s", rec.Code, rec.Body)
	}
	// Pemeriksaan manual dicatat dan memicu email.
	if rec := h.do(http.MethodPost, "/api/v1/updates/check", nil); rec.Code != http.StatusOK {
		t.Fatalf("periksa: %d %s", rec.Code, rec.Body)
	}
	rec := h.do(http.MethodGet, "/api/v1/updates/schedule", nil)
	if !strings.Contains(rec.Body.String(), `"hour":2`) || !strings.Contains(rec.Body.String(), `"name":"sapo"`) {
		t.Fatalf("info: %s", rec.Body)
	}
	ctx := context.Background()
	h.s.notifyCheck(ctx)
	if sent.count() != 1 || !strings.Contains(sent.last().Body, "Ada 2 pembaruan sistem") {
		t.Fatalf("email pembaruan: %d %+v", sent.count(), sent.last())
	}
	// Sudah dipasang (0 pembaruan): tanpa email "pulih".
	agent.mu.Lock()
	agent.results[proto.OpUpdatesCheck] = proto.UpdatesCheckResult{CheckedAt: time.Now()}
	agent.mu.Unlock()
	if _, err := h.s.runUpdateCheck(ctx); err != nil {
		t.Fatal(err)
	}
	h.s.notifyCheck(ctx)
	if sent.count() != 1 {
		t.Fatalf("tidak boleh ada email pulih untuk pembaruan: %+v", sent.last())
	}
}
