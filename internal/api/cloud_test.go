package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/proto"
)

func TestCloudAPI(t *testing.T) {
	agent := shareAgent()
	agent.results[proto.OpCloudRepoAdd] = proto.CloudRepoAdded{ID: "abc123", Password: "AAAA-BBBB"}
	agent.results[proto.OpCloudJobSet] = map[string]string{"id": "keluarga-cabc123"}
	agent.results[proto.OpCloudRestore] = map[string]bool{"ok": true}
	agent.results[proto.OpCloudList] = proto.CloudListResult{Available: true, Repos: []proto.CloudRepo{{ID: "abc123", Name: "B2"}},
		Jobs: []proto.CloudJobStatus{{CloudJob: proto.CloudJob{ID: "keluarga-cabc123", Repo: "abc123", Share: "keluarga"}, LastError: "Access Denied"}}}
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
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "keluarga", "pool": "tank", "permissions": map[string]string{}, "admin_password": adminPassword})

	repo := map[string]any{"name": "B2", "endpoint": "https://s3.example.com/", "bucket": "sapo", "access_key": "AKIA-RAHASIA", "secret_key": "kunci-rahasia-s3"}
	if rec := h.do(http.MethodPost, "/api/v1/cloud/repos", repo); rec.Code == http.StatusOK {
		t.Fatal("tanpa password admin harus ditolak")
	}
	repo["admin_password"] = adminPassword
	rec := h.do(http.MethodPost, "/api/v1/cloud/repos", repo)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "AAAA-BBBB") {
		t.Fatalf("repo: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpCloudRepoAdd).(proto.CloudRepoParams); p.Endpoint != "s3.example.com" || p.SecretKey != "kunci-rahasia-s3" {
		t.Fatalf("cloud.repo_add = %+v", p)
	}
	// Audit tanpa rahasia.
	logs, _ := os.ReadFile(filepath.Join(h.data, "audit.log"))
	if !strings.Contains(string(logs), "cloud.repo_add") {
		t.Fatal("cloud.repo_add tidak tercatat di audit")
	}
	if strings.Contains(string(logs), "kunci-rahasia-s3") || strings.Contains(string(logs), "AKIA-RAHASIA") || strings.Contains(string(logs), "AAAA-BBBB") {
		t.Fatalf("rahasia masuk log audit:\n%s", logs)
	}
	if rec := h.do(http.MethodPost, "/api/v1/cloud/jobs", map[string]any{"repo": "abc123", "share": "keluarga", "hour": 3, "keep_days": 30, "enabled": true, "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("tugas: %d %s", rec.Code, rec.Body)
	}
	if p, _ := agent.param(proto.OpCloudJobSet).(proto.CloudJob); p.Pool != "tank" || p.Share != "keluarga" || p.KeepDays != 30 {
		t.Fatalf("cloud.job_set = %+v", p)
	}
	if rec := h.do(http.MethodPost, "/api/v1/cloud/repos/abc123/restore", map[string]any{"snapshot": "0123abcd", "pool": "tank", "name": "keluarga", "admin_password": adminPassword}); errCode(rec) != "share_taken" {
		t.Fatalf("nama terpakai: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/cloud/repos/abc123/restore", map[string]any{"snapshot": "0123abcd", "pool": "tank", "name": "dari-cloud", "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("pulihkan: %d %s", rec.Code, rec.Body)
	}
	// Backup cloud gagal: email; sumber cloud tidak terbaca bukan "pulih".
	sent := &sentMail{}
	h.s.notify.send = sent.send
	h.do(http.MethodPost, "/api/v1/notify", map[string]any{"enabled": true, "host": "127.0.0.1", "port": 2525, "security": "none", "from": "sapo@uji.local", "to": []string{"admin@uji.local"}, "admin_password": adminPassword})
	h.s.notifyCheck(context.Background())
	if sent.count() != 1 || !strings.Contains(sent.last().Body, "Backup keluarga → B2 gagal: Access Denied") {
		t.Fatalf("email: %d %+v", sent.count(), sent.last())
	}
	agent.setErrs(map[string]error{proto.OpCloudList: &proto.Error{Code: proto.ErrUnavailable, Message: "x"}})
	h.s.notifyCheck(context.Background())
	agent.setErrs(nil)
	if sent.count() != 1 {
		t.Fatalf("galat baca cloud bukan pulih: %+v", sent.last())
	}
}

func TestCloudWizardsLoad(t *testing.T) {
	for _, id := range []string{"cloud-repo-add", "cloud-repo-connect", "cloud-job-create"} {
		if _, err := LoadWizard("../../wizards", id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
}
