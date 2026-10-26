package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestTimeMachineAPI(t *testing.T) {
	agent := shareAgent()
	h := newHarness(t, agent)
	h.runSetup()
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "keluarga", "pool": "tank", "permissions": map[string]string{}, "admin_password": adminPassword})
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/time-machine", map[string]any{"enabled": true, "quota_gb": 500}); rec.Code == http.StatusOK {
		t.Fatal("tanpa password admin harus ditolak")
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/time-machine", map[string]any{"enabled": true, "quota_gb": 5, "admin_password": adminPassword}); rec.Code != http.StatusBadRequest {
		t.Fatalf("kuota terlalu kecil: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/time-machine", map[string]any{"enabled": true, "quota_gb": 500, "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("nyalakan: %d %s", rec.Code, rec.Body)
	}
	// Tempat sampah dimatikan; spesifikasi diteruskan ke sapod.
	if p, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams); len(p.Shares) != 1 || p.Shares[0].TimeMachine == nil || p.Shares[0].TimeMachine.QuotaGB != 500 || p.Shares[0].RecycleDays != 0 {
		t.Fatalf("shares.apply = %+v", p)
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/recycle", map[string]any{"days": 30, "admin_password": adminPassword}); errCode(rec) != "time_machine_share" {
		t.Fatalf("tempat sampah di folder Time Machine: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/shares", nil); !strings.Contains(rec.Body.String(), `"time_machine":{"quota_gb":500}`) {
		t.Fatalf("daftar: %s", rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/time-machine", map[string]any{"enabled": false, "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("matikan: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams); p.Shares[0].TimeMachine != nil {
		t.Fatalf("shares.apply = %+v", p)
	}
	if logs, _ := os.ReadFile(filepath.Join(h.data, "audit.log")); strings.Count(string(logs), "shares.time_machine") != 2 {
		t.Fatalf("audit:\n%s", logs)
	}
}
