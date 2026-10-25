package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/mail"
	"github.com/mehuli-id/sapo/internal/proto"
)

type sentMail struct {
	mu   sync.Mutex
	list []mail.Message
	srv  []mail.Server
	fail error
}

func (m *sentMail) send(_ context.Context, s mail.Server, msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.list, m.srv = append(m.list, msg), append(m.srv, s)
	return nil
}

func (m *sentMail) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.list)
}

func (m *sentMail) last() mail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list[len(m.list)-1]
}

func TestNotify(t *testing.T) {
	agent := shareAgent()
	agent.results[proto.OpStoragePools] = proto.PoolsResult{Available: true, Pools: []proto.Pool{{Name: "tank", Health: "DEGRADED", CapacityPct: 95}}}
	agent.results[proto.OpStorageDisks] = []proto.Disk{}
	agent.results[proto.OpBackupsList] = proto.BackupsListResult{Jobs: []proto.BackupStatus{{BackupJob: proto.BackupJob{ID: "kel-usb", Share: "kel", DestPool: "usb"}, LastError: "pool tujuan tidak ditemukan"}}}
	h := newHarness(t, agent)
	shipped, err := config.Load("../../config/sapo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	h.s.cfg.API.Notify.Messages = shipped.API.Notify.Messages
	h.runSetup()
	sent := &sentMail{}
	h.s.notify.send = sent.send

	base := map[string]any{"enabled": true, "host": "127.0.0.1", "port": 2525, "security": "none", "from": "sapo@uji.local", "to": []string{"admin@uji.local"}, "admin_password": adminPassword}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	for name, c := range map[string]struct {
		body map[string]any
		code string
	}{
		"login tanpa enkripsi": {with("username", "budi", "password", "x"), "smtp_invalid"},
		"penerima aneh":        {with("to", []string{"a@b.c\r\nBcc: x@y.z"}), "email_invalid"},
		"tanpa penerima":       {with("to", []string{}), "email_recipients"},
		"pengirim aneh":        {with("from", "Sapo <s@x.y>"), "email_invalid"},
	} {
		if rec := h.do(http.MethodPost, "/api/v1/notify", c.body); errCode(rec) != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := h.do(http.MethodPost, "/api/v1/notify", with("admin_password", "salah-sekali")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("password salah: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/api/v1/notify", base); rec.Code != http.StatusOK {
		t.Fatalf("simpan: %d %s", rec.Code, rec.Body)
	}

	// Tiga masalah baru dalam satu email.
	ctx := context.Background()
	h.s.notifyCheck(ctx)
	if sent.count() != 1 {
		t.Fatalf("email = %d", sent.count())
	}
	m := sent.last()
	if !strings.Contains(m.Subject, "3 masalah") || !strings.Contains(m.Body, "Pool tank tidak sehat (DEGRADED)") ||
		!strings.Contains(m.Body, "terisi 95%") || !strings.Contains(m.Body, "Backup kel → usb gagal: pool tujuan tidak ditemukan") {
		t.Fatalf("email:\n%s\n%s", m.Subject, m.Body)
	}
	// Tidak diulang.
	h.s.notifyCheck(ctx)
	if sent.count() != 1 {
		t.Fatalf("masalah yang sama dikirim ulang: %d", sent.count())
	}
	// sapod tidak bisa dibaca: masalah tidak dianggap pulih.
	agent.setErrs(map[string]error{proto.OpStoragePools: errors.New("putus")})
	h.s.notifyCheck(ctx)
	agent.setErrs(nil)
	if sent.count() != 1 {
		t.Fatal("galat baca tidak boleh dianggap pulih")
	}
	// Pool pulih: email "pulih".
	agent.mu.Lock()
	agent.results[proto.OpStoragePools] = proto.PoolsResult{Available: true, Pools: []proto.Pool{{Name: "tank", Health: "ONLINE", CapacityPct: 50}}}
	agent.mu.Unlock()
	h.s.notifyCheck(ctx)
	if sent.count() != 2 || !strings.Contains(sent.last().Subject, "pulih") || !strings.Contains(sent.last().Body, "Sudah pulih") || strings.Contains(sent.last().Body, "Backup kel") {
		t.Fatalf("email pulih:\n%+v", sent.last())
	}
	// Gagal kirim: dicoba lagi, galat terlihat di panel.
	agent.mu.Lock()
	agent.results[proto.OpStoragePools] = proto.PoolsResult{Available: true, Pools: []proto.Pool{{Name: "tank", Health: "FAULTED"}}}
	agent.mu.Unlock()
	sent.mu.Lock()
	sent.fail = errors.New("server menolak")
	sent.mu.Unlock()
	h.s.notifyCheck(ctx)
	rec := h.do(http.MethodGet, "/api/v1/notify", nil)
	if !strings.Contains(rec.Body.String(), "server menolak") {
		t.Fatalf("galat kirim: %s", rec.Body)
	}
	sent.mu.Lock()
	sent.fail = nil
	sent.mu.Unlock()
	h.s.notifyCheck(ctx)
	if sent.count() != 3 || !strings.Contains(sent.last().Body, "FAULTED") {
		t.Fatalf("kirim ulang: %d %+v", sent.count(), sent.last())
	}
	// Kejadian yang dimatikan tidak dikirim.
	h.do(http.MethodPost, "/api/v1/notify", with("events_off", []string{"pool_full"}, "capacity_pct", 10))
	h.s.notifyCheck(ctx)
	if strings.Contains(sent.last().Body, "terisi") {
		t.Fatal("pool_full dimatikan")
	}

	// Password SMTP: tidak pernah dikembalikan; tetap dipakai hanya untuk
	// server dan username yang sama.
	tls := with("security", "starttls", "port", 587, "host", "smtp.contoh.id", "username", "budi", "password", "rahasia-smtp")
	if rec := h.do(http.MethodPost, "/api/v1/notify", tls); rec.Code != http.StatusOK {
		t.Fatalf("starttls: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/notify", nil); strings.Contains(rec.Body.String(), "rahasia-smtp") || !strings.Contains(rec.Body.String(), `"has_password":true`) {
		t.Fatalf("password bocor: %s", rec.Body)
	}
	tls["password"] = ""
	if rec := h.do(http.MethodPost, "/api/v1/notify", tls); rec.Code != http.StatusOK {
		t.Fatalf("simpan tanpa ganti password: %d %s", rec.Code, rec.Body)
	}
	if h.s.notify.get().Settings.Password != "rahasia-smtp" {
		t.Fatal("password tersimpan harus tetap")
	}
	tls["host"] = "smtp.lain.id"
	if rec := h.do(http.MethodPost, "/api/v1/notify", tls); errCode(rec) != "smtp_password_required" {
		t.Fatalf("server baru tanpa password: %d %s", rec.Code, rec.Body)
	}
	// Email uji.
	before := sent.count()
	if rec := h.do(http.MethodPost, "/api/v1/notify/test", nil); rec.Code != http.StatusOK {
		t.Fatalf("email uji: %d %s", rec.Code, rec.Body)
	}
	if sent.count() != before+1 || !strings.Contains(sent.last().Subject, "Email uji") {
		t.Fatalf("email uji: %+v", sent.last())
	}
	sent.mu.Lock()
	if s := sent.srv[len(sent.srv)-1]; s.Host != "smtp.contoh.id" || s.Password != "rahasia-smtp" {
		t.Fatalf("server email uji = %+v", s)
	}
	sent.mu.Unlock()
}

func TestNotifyWizardLoads(t *testing.T) {
	wz, err := LoadWizard("../../wizards", "notify-setup")
	if err != nil || wz.Action != "notify.setup" || !wz.AdminOnly {
		t.Fatalf("wizard = %+v, %v", wz, err)
	}
}

// Disk yang tidur, tidak terbaca, atau hilang tidak dianggap pulih; pool
// penuh pulih dengan histeresis; sapod lama bukan "pulih".
func TestNotifyNoFalseResolve(t *testing.T) {
	agent := shareAgent()
	agent.results[proto.OpStoragePools] = proto.PoolsResult{Available: true, Pools: []proto.Pool{{Name: "tank", Health: "ONLINE", CapacityPct: 91}}}
	agent.results[proto.OpStorageDisks] = []proto.Disk{{Name: "sda", Serial: "S1", Model: "Disk"}}
	agent.results[proto.OpStorageSMART] = proto.SMART{Device: "sda", Status: proto.SMARTWarning}
	agent.results[proto.OpBackupsList] = proto.BackupsListResult{Jobs: []proto.BackupStatus{{BackupJob: proto.BackupJob{ID: "kel-usb", Share: "kel", DestPool: "usb"}, LastError: "gagal"}}}
	h := newHarness(t, agent)
	shipped, err := config.Load("../../config/sapo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	h.s.cfg.API.Notify.Messages = shipped.API.Notify.Messages
	h.s.cfg.API.Notify.SMARTMinutes = 0
	h.runSetup()
	sent := &sentMail{}
	h.s.notify.send = sent.send
	if rec := h.do(http.MethodPost, "/api/v1/notify", map[string]any{"enabled": true, "host": "127.0.0.1", "port": 2525, "security": "none", "from": "sapo@uji.local", "to": []string{"admin@uji.local"}, "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("simpan: %d %s", rec.Code, rec.Body)
	}
	ctx := context.Background()
	h.s.notifyCheck(ctx)
	if sent.count() != 1 || !strings.Contains(sent.last().Subject, "3 masalah") {
		t.Fatalf("awal: %d %+v", sent.count(), sent.last())
	}
	set := func(op string, v any) {
		agent.mu.Lock()
		agent.results[op] = v
		agent.mu.Unlock()
	}
	// Disk tidur, kapasitas turun ke 89 (masih di atas ambang−2), sapod lama
	// tanpa backups.list: tidak ada yang pulih.
	set(proto.OpStorageSMART, proto.SMART{Device: "sda", Status: proto.SMARTStandby})
	set(proto.OpStoragePools, proto.PoolsResult{Available: true, Pools: []proto.Pool{{Name: "tank", Health: "ONLINE", CapacityPct: 89}}})
	agent.setErrs(map[string]error{proto.OpBackupsList: &proto.Error{Code: proto.ErrUnknownOp, Message: "x"}})
	h.s.notifyCheck(ctx)
	if sent.count() != 1 {
		t.Fatalf("tidak boleh ada email pulih palsu: %+v", sent.last())
	}
	// Disk hilang dari daftar: tetap tidak pulih.
	set(proto.OpStorageDisks, []proto.Disk{})
	h.s.notifyCheck(ctx)
	if sent.count() != 1 {
		t.Fatalf("disk hilang bukan pulih: %+v", sent.last())
	}
	// Disk kembali sehat, kapasitas di bawah ambang−2, backup terbaca: pulih.
	agent.setErrs(nil)
	set(proto.OpStorageDisks, []proto.Disk{{Name: "sda", Serial: "S1", Model: "Disk"}})
	set(proto.OpStorageSMART, proto.SMART{Device: "sda", Status: proto.SMARTPassed})
	set(proto.OpStoragePools, proto.PoolsResult{Available: true, Pools: []proto.Pool{{Name: "tank", Health: "ONLINE", CapacityPct: 87}}})
	set(proto.OpBackupsList, proto.BackupsListResult{Jobs: []proto.BackupStatus{}})
	h.s.notifyCheck(ctx)
	if sent.count() != 2 || strings.Count(sent.last().Body, "\n- ") != 3 {
		t.Fatalf("pulih: %d\n%s", sent.count(), sent.last().Body)
	}
}
