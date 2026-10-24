package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

const adminPassword = "lumbung padi di bukit"

func poolRequest(confirm, password string) poolCreateRequest {
	return poolCreateRequest{
		Name: "data", Layout: proto.LayoutMirror, ConfirmName: confirm, Password: password,
		Disks: []proto.DiskRef{{Name: "vdb", Serial: "A", SizeBytes: 1 << 32}, {Name: "vdc", Serial: "B", SizeBytes: 1 << 32, Erase: true}},
	}
}

func TestPoolCreateNeedsPasswordAndConfirmation(t *testing.T) {
	agent := &fakeAgent{results: map[string]any{proto.OpStoragePoolCreate: proto.Pool{Name: "data", Health: "ONLINE"}}}
	h := newHarness(t, agent)
	if rec := h.do(http.MethodPost, "/api/v1/storage/pools", poolRequest("data", adminPassword)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa login harus 401, dapat %d", rec.Code)
	}
	h.runSetup()

	// Nama yang diketik ulang berbeda, atau password salah: sapod tidak dipanggil.
	if rec := h.do(http.MethodPost, "/api/v1/storage/pools", poolRequest("dta", adminPassword)); rec.Code != http.StatusBadRequest || errCode(rec) != "confirm_mismatch" {
		t.Fatalf("konfirmasi salah: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/storage/pools", poolRequest("data", "password salah sekali")); rec.Code != http.StatusUnauthorized || errCode(rec) != "bad_password" {
		t.Fatalf("password salah: %d %s", rec.Code, rec.Body)
	}
	if _, called := agent.params[proto.OpStoragePoolCreate]; called {
		t.Fatal("sapod tidak boleh dipanggil sebelum password dan konfirmasi benar")
	}
	// Tanpa header CSRF ditolak.
	if rec := h.do(http.MethodPost, "/api/v1/storage/pools", poolRequest("data", adminPassword), noCSRF()); rec.Code != http.StatusForbidden {
		t.Fatalf("tanpa CSRF: %d", rec.Code)
	}

	rec := h.do(http.MethodPost, "/api/v1/storage/pools", poolRequest("data", adminPassword))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"health":"ONLINE"`) {
		t.Fatalf("buat pool: %d %s", rec.Code, rec.Body)
	}
	got, _ := agent.params[proto.OpStoragePoolCreate].(proto.PoolCreateParams)
	if got.Name != "data" || got.Layout != proto.LayoutMirror || len(got.Disks) != 2 || got.Disks[0].Erase || !got.Disks[1].Erase {
		t.Fatalf("parameter ke sapod = %+v", got)
	}

	// Permintaan dicatat sebelum sapod dipanggil, hasilnya sesudahnya; password tidak pernah.
	b, _ := os.ReadFile(filepath.Join(h.data, "audit.log"))
	log := string(b)
	req := strings.Index(log, `"action":"storage.pool_create.request"`)
	done := strings.Index(log, `"action":"storage.pool_create","ip"`)
	if req < 0 || done < req || !strings.Contains(log, `"erase":"vdc"`) || strings.Contains(log, adminPassword) {
		t.Fatalf("log audit:\n%s", log)
	}
}

func TestPoolCreateConflictsAndAuditOfFailure(t *testing.T) {
	agent := &fakeAgent{
		results: map[string]any{},
		errs:    map[string]error{proto.OpStoragePoolCreate: &proto.Error{Code: proto.ErrDiskHasData, Message: "disk vdc berisi data"}},
	}
	h := newHarness(t, agent)
	h.runSetup()
	rec := h.do(http.MethodPost, "/api/v1/storage/pools", poolRequest("data", adminPassword))
	if rec.Code != http.StatusConflict || errCode(rec) != proto.ErrDiskHasData {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	b, _ := os.ReadFile(filepath.Join(h.data, "audit.log"))
	if !strings.Contains(string(b), `"ok":false,"detail":{"disks":"vdb,vdc","erase":"vdc","error":"disk_has_data"`) {
		t.Fatalf("kegagalan harus tercatat:\n%s", b)
	}
	if rec := h.do(http.MethodPost, "/api/v1/storage/pools", poolCreateRequest{Name: "data", ConfirmName: "data", Password: adminPassword}); rec.Code != http.StatusBadRequest {
		t.Fatalf("tanpa disk: %d", rec.Code)
	}
}

func TestPoolCreateRefusedWithoutAudit(t *testing.T) {
	agent := &fakeAgent{results: map[string]any{proto.OpStoragePoolCreate: proto.Pool{Name: "data"}}}
	h := newHarness(t, agent)
	h.runSetup()
	// Log audit tidak bisa ditulis lagi.
	if err := h.s.audit.Close(); err != nil {
		t.Fatal(err)
	}
	rec := h.do(http.MethodPost, "/api/v1/storage/pools", poolRequest("data", adminPassword))
	if rec.Code != http.StatusInternalServerError || errCode(rec) != "audit_unavailable" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if _, called := agent.params[proto.OpStoragePoolCreate]; called {
		t.Fatal("pool tidak boleh dibuat tanpa log audit")
	}
}

func TestPoolListAndScrub(t *testing.T) {
	agent := &fakeAgent{results: map[string]any{
		proto.OpStoragePools:     proto.PoolsResult{Available: true, Pools: []proto.Pool{{Name: "tank"}}},
		proto.OpStoragePoolScrub: map[string]bool{"ok": true},
	}}
	h := newHarness(t, agent)
	h.runSetup()
	if rec := h.do(http.MethodGet, "/api/v1/storage/pools", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tank"`) {
		t.Fatalf("daftar pool: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/storage/pools/tank/scrub", map[string]string{"action": "start"}); rec.Code != http.StatusOK {
		t.Fatalf("scrub: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.params[proto.OpStoragePoolScrub].(proto.PoolScrubParams); p.Name != "tank" || p.Action != "start" {
		t.Fatalf("parameter scrub = %+v", agent.params[proto.OpStoragePoolScrub])
	}
	b, _ := os.ReadFile(filepath.Join(h.data, "audit.log"))
	if !strings.Contains(string(b), `"action":"storage.pool_scrub"`) {
		t.Fatalf("scrub harus tercatat:\n%s", b)
	}
	agent.errs = map[string]error{proto.OpStoragePoolScrub: &proto.Error{Code: proto.ErrPoolBusy, Message: "resilver"}}
	if rec := h.do(http.MethodPost, "/api/v1/storage/pools/tank/scrub", map[string]string{"action": "start"}); rec.Code != http.StatusConflict {
		t.Fatalf("pool sibuk: %d", rec.Code)
	}
}

func TestPoolWizardLoads(t *testing.T) {
	wz, err := LoadWizard("../../wizards", "pool-create")
	if err != nil {
		t.Fatal(err)
	}
	if wz.Action != "storage.pool_create" || !wz.Destructive || len(wz.Steps) != 4 {
		t.Fatalf("wizard = %+v", wz)
	}
	// source wajib merujuk isian disk-select.
	dir := t.TempDir()
	bad := strings.Replace(mustRead(t, "../../wizards/pool-create.yaml"), "source: disks\n        required: true\n        label: { id: Susunan", "source: name\n        required: true\n        label: { id: Susunan", 1)
	if err := os.WriteFile(filepath.Join(dir, "pool-create.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWizard(dir, "pool-create"); err == nil {
		t.Fatal("source yang bukan disk-select harus ditolak")
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
