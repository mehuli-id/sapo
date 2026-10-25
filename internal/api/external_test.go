package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestExternalAPI(t *testing.T) {
	agent := shareAgent()
	agent.results[proto.OpExternalList] = []proto.ExternalVolume{{Name: "usb", Group: "ortu", ReadOnly: true, State: "mounted", Label: "FOTO"}}
	agent.results[proto.OpExternalMount] = map[string]bool{"ok": true}
	agent.results[proto.OpExternalUnmount] = map[string]bool{"ok": true}
	agent.results[proto.OpFilesList] = proto.FilesListResult{}
	h := newHarness(t, agent)
	h.runSetup()
	h.do(http.MethodPost, "/api/v1/groups", map[string]string{"name": "ortu"})
	h.createUser("budi", "user", false, "ortu")
	h.createUser("ani", "user", false)

	body := map[string]any{"uuid": "AAAA-1111", "name": "usb2", "group": "ortu", "read_only": false, "smb": true}
	if rec := h.do(http.MethodPost, "/api/v1/storage/external/mount", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa password: %d", rec.Code)
	}
	body["admin_password"] = adminPassword
	if rec := h.do(http.MethodPost, "/api/v1/storage/external/mount", body); rec.Code != http.StatusOK {
		t.Fatalf("pasang: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpExternalMount).(proto.ExternalMountParams); p != (proto.ExternalMountParams{UUID: "AAAA-1111", Name: "usb2", Group: "ortu", SMB: true}) {
		t.Fatalf("external.mount = %+v", p)
	}
	for _, bad := range []map[string]any{
		{"uuid": "AAAA-1111", "name": "usb3", "group": "hantu", "admin_password": adminPassword},
		{"uuid": "AAAA-1111", "name": "homes", "group": "ortu", "admin_password": adminPassword},
	} {
		if rec := h.do(http.MethodPost, "/api/v1/storage/external/mount", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%v harus ditolak: %d", bad, rec.Code)
		}
	}
	// Nama folder eksternal tidak bisa dipakai shared folder.
	if rec := h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "usb", "pool": "tank", "permissions": map[string]string{}, "admin_password": adminPassword}); errCode(rec) != "share_taken" {
		t.Fatalf("shared folder bernama sama: %d %s", rec.Code, rec.Body)
	}

	// File Station: budi (anggota ortu) melihat folder eksternal, baca saja;
	// ani tidak.
	h.loginAs("budi", userPassword)
	rec := h.do(http.MethodGet, "/api/v1/files/shares", nil)
	if !strings.Contains(rec.Body.String(), `"name":"usb","description":"FOTO","access":"read","external":true`) {
		t.Fatalf("folder budi: %s", rec.Body)
	}
	h.do(http.MethodGet, "/api/v1/files/list?share=usb&path=a", nil)
	if p, _ := agent.param(proto.OpFilesList).(proto.FilesTarget); p != (proto.FilesTarget{User: "budi", Share: "usb", External: true, Path: "a"}) {
		t.Fatalf("files.list = %+v", p)
	}
	if rec := h.do(http.MethodPost, "/api/v1/files/mkdir", map[string]string{"share": "usb", "path": "baru"}); rec.Code != http.StatusForbidden {
		t.Fatalf("tulis ke folder eksternal baca saja: %d", rec.Code)
	}
	h.loginAs("ani", userPassword)
	if rec := h.do(http.MethodGet, "/api/v1/files/shares", nil); strings.Contains(rec.Body.String(), `"usb"`) {
		t.Fatalf("ani tidak boleh melihat folder eksternal: %s", rec.Body)
	}
}
