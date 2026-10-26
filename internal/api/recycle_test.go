package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestRecycleAPI(t *testing.T) {
	agent := shareAgent()
	agent.results[proto.OpRecycleStatus] = []proto.RecycleUsage{{Pool: "tank", Share: "keluarga", Days: 30, Bytes: 2048, Files: 3}}
	agent.results[proto.OpRecycleEmpty] = proto.RecycleEmptyResult{Removed: 3, Done: true}
	h := newHarness(t, agent)
	h.runSetup()
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "keluarga", "pool": "tank", "permissions": map[string]string{}, "admin_password": adminPassword})
	// Folder baru: tempat sampah menyala dengan lama simpan bawaan.
	if sh, _ := h.s.shares.get("keluarga"); sh.RecycleDays != 30 {
		t.Fatalf("lama simpan bawaan = %d", sh.RecycleDays)
	}
	if p, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams); len(p.Shares) != 1 || p.Shares[0].RecycleDays != 30 {
		t.Fatalf("shares.apply = %+v", p)
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/recycle", map[string]any{"days": 7}); rec.Code == http.StatusOK {
		t.Fatal("tanpa password admin harus ditolak")
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/recycle", map[string]any{"days": 366, "admin_password": adminPassword}); rec.Code != http.StatusBadRequest {
		t.Fatalf("lama simpan di luar batas: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/recycle", map[string]any{"days": 7, "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("lama simpan: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpSharesApply).(proto.SharesApplyParams); p.Shares[0].RecycleDays != 7 {
		t.Fatalf("shares.apply = %+v", p)
	}
	if rec := h.do(http.MethodGet, "/api/v1/recycle", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"bytes":2048`) {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/recycle/empty", map[string]any{"confirm_name": "lain", "admin_password": adminPassword}); errCode(rec) != "confirm_mismatch" {
		t.Fatalf("nama salah: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/recycle/empty", map[string]any{"confirm_name": "keluarga"}); rec.Code == http.StatusOK {
		t.Fatal("kosongkan tanpa password harus ditolak")
	}
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/recycle/empty", map[string]any{"confirm_name": "keluarga", "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("kosongkan: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpRecycleEmpty).(proto.RecycleTarget); p != (proto.RecycleTarget{Pool: "tank", Share: "keluarga"}) {
		t.Fatalf("recycle.empty = %+v", p)
	}
	logs, _ := os.ReadFile(filepath.Join(h.data, "audit.log"))
	if !strings.Contains(string(logs), "recycle.empty") || !strings.Contains(string(logs), "recycle.policy") {
		t.Fatalf("audit:\n%s", logs)
	}
}
