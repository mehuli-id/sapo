package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/proto"
)

// overTLS: permintaan admin seolah lewat HTTPS (cookie sesi aman).
func overTLS(r *http.Request) {
	r.TLS = &tls.ConnectionState{}
	if c, err := r.Cookie(SessionCookie); err == nil {
		r.AddCookie(&http.Cookie{Name: SecureSessionCookie, Value: c.Value})
	}
}

func TestRemoteBackupAPI(t *testing.T) {
	agent := shareAgent()
	dir := t.TempDir()
	sendFile, recvFile := filepath.Join(dir, "kirim"), filepath.Join(dir, "terima")
	_ = os.WriteFile(sendFile, []byte("ALIRAN-ZFS"), 0o600)
	_ = os.WriteFile(recvFile, nil, 0o600)
	snap := "sapo-rbackup-keluarga-r_GMT-2026.10.28-02.00.00"
	agent.files = map[string]string{proto.OpRBackupSendOpen: sendFile, proto.OpRBackupRecvOpen: recvFile}
	agent.results[proto.OpRBackupSendOpen] = proto.RBackupSession{Session: "s1", Snap: snap}
	agent.results[proto.OpRBackupRecvOpen] = proto.RBackupSession{Session: "r1"}
	agent.results[proto.OpRBackupRecvFinish] = map[string]bool{"ok": true}
	agent.results[proto.OpRBackupSendFinish] = map[string]bool{"ok": true}
	agent.results[proto.OpRBackupList] = proto.RBackupList{Copies: []proto.BackupCopy{{Name: snap}}}
	agent.results[proto.OpSystemInfo] = proto.SystemInfo{Hostname: "nas-tujuan"}
	h := newHarness(t, agent)
	h.runSetup()
	h.do(http.MethodPost, "/api/v1/shares", map[string]any{"name": "keluarga", "pool": "tank", "permissions": map[string]string{}, "admin_password": adminPassword})

	// NAS penerima: server HTTPS sungguhan (sertifikat httptest).
	ts := httptest.NewTLSServer(h.s)
	defer ts.Close()
	h.s.https = &HTTPSInfo{Port: "5001", Fingerprint: func() string { return "" }, Certificate: func() []byte { return ts.Certificate().Raw }}
	rec := h.do(http.MethodPost, "/api/v1/backup-partners", map[string]any{"name": "NAS kantor", "pool": "usb", "keep": 14, "quota_gb": 2, "admin_password": adminPassword}, overTLS)
	if rec.Code != http.StatusOK {
		t.Fatalf("partner: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID, Code string }
	decode(t, rec, &created)
	if !pairingCodeRe.MatchString(created.Code) || strings.Contains(h.s.partners.all()[0].TokenHash, strings.Split(created.Code, "-")[2]) {
		t.Fatalf("kode = %q", created.Code)
	}
	h.s.https = nil
	addr := strings.TrimPrefix(ts.URL, "https://")

	// Token salah ditolak.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/rbackup/hello", nil)
	req.Header.Set("Authorization", partnerAuthScheme+created.ID+":salah")
	res, err := ts.Client().Do(req)
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token salah: %v %v", res, err)
	}
	res.Body.Close()

	// Kode dengan sidik jari lain: pemasangan ditolak.
	bad := created.Code[:len(created.Code)-32] + strings.Repeat("0", 32)
	if rec := h.do(http.MethodPost, "/api/v1/remote-backups/targets", map[string]any{"name": "Kantor", "address": addr, "code": bad, "admin_password": adminPassword}); errCode(rec) != "pairing_failed" {
		t.Fatalf("sidik jari salah: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/remote-backups/targets", map[string]any{"name": "Kantor", "address": addr, "code": created.Code, "admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("pasangkan: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodGet, "/api/v1/remote-backups", nil); strings.Contains(rec.Body.String(), strings.Split(created.Code, "-")[2]) || !strings.Contains(rec.Body.String(), "nas-tujuan") {
		t.Fatalf("token bocor ke panel atau host tidak terbaca: %s", rec.Body)
	}
	rec = h.do(http.MethodPost, "/api/v1/remote-backups/jobs", map[string]any{"share": "keluarga", "target": created.ID, "hour": 2, "enabled": true, "admin_password": adminPassword})
	if rec.Code != http.StatusOK {
		t.Fatalf("tugas: %d %s", rec.Code, rec.Body)
	}
	var job struct{ ID string }
	decode(t, rec, &job)

	if err := h.s.runRemoteJob(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(recvFile); string(got) != "ALIRAN-ZFS" {
		t.Fatalf("data di penerima = %q", got)
	}
	if p, _ := agent.param(proto.OpRBackupRecvOpen).(proto.RBackupRecvParams); p.Pool != "usb" || p.Partner != created.ID || p.Job != job.ID || p.Snap != snap || p.QuotaBytes != 2<<30 {
		t.Fatalf("recv_open = %+v", p)
	}
	if p, _ := agent.param(proto.OpRBackupRecvFinish).(proto.RBackupRecvFinish); !p.OK || p.KeepDays != 14 {
		t.Fatalf("recv_finish = %+v (jumlah simpan dari penerima)", p)
	}
	if p, _ := agent.param(proto.OpRBackupSendFinish).(proto.RBackupSendFinish); !p.OK || !p.RemoteHas {
		t.Fatalf("send_finish = %+v", p)
	}
	if j := h.s.remote.get().Jobs[0]; !j.LastOK || j.LastBytes != int64(len("ALIRAN-ZFS")) || j.LastCopies != 1 {
		t.Fatalf("hasil tugas = %+v", j)
	}
	// Folder yang di-backup dan tujuan yang dipakai tidak bisa dihapus.
	if rec := h.do(http.MethodPost, "/api/v1/shares/keluarga/delete", map[string]any{"confirm_name": "keluarga", "admin_password": adminPassword}); rec.Code != http.StatusConflict {
		t.Fatalf("hapus folder yang di-backup: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do(http.MethodPost, "/api/v1/remote-backups/targets/"+created.ID+"/delete", nil); errCode(rec) != "target_used" {
		t.Fatalf("hapus tujuan yang dipakai: %d %s", rec.Code, rec.Body)
	}
	// Sertifikat tujuan berganti: backup berhenti (tidak mengirim ke server lain).
	ts2 := httptest.NewUnstartedServer(h.s)
	ts2.TLS = &tls.Config{Certificates: []tls.Certificate{otherCert(t)}}
	ts2.StartTLS()
	defer ts2.Close()
	_ = h.s.remote.mutate(func(v *remoteFile) error {
		v.Targets[0].Address = strings.TrimPrefix(ts2.URL, "https://")
		return nil
	})
	if err := h.s.runRemoteJob(context.Background(), job.ID); err == nil || !strings.Contains(err.Error(), "sertifikat") {
		t.Fatalf("sertifikat berganti: %v", err)
	}
	// Partner dicabut: token tidak berlaku lagi.
	if rec := h.do(http.MethodPost, "/api/v1/backup-partners/"+created.ID+"/delete", map[string]any{"admin_password": adminPassword}); rec.Code != http.StatusOK {
		t.Fatalf("cabut partner: %d %s", rec.Code, rec.Body)
	}
	_ = h.s.remote.mutate(func(v *remoteFile) error {
		v.Targets[0].Address = addr
		return nil
	})
	if err := h.s.runRemoteJob(context.Background(), job.ID); err == nil || !strings.Contains(err.Error(), "unauthorized") && !strings.Contains(err.Error(), "partner") {
		t.Fatalf("partner dicabut: %v", err)
	}
	// Partner yang dicabut tetap tampil (salinannya bisa dipulihkan/dihapus).
	if rec := h.do(http.MethodGet, "/api/v1/backup-partners", nil); !strings.Contains(rec.Body.String(), `"revoked":true`) {
		t.Fatalf("partner dicabut harus tetap tampil: %s", rec.Body)
	}
}

func TestNormalizeAddress(t *testing.T) {
	for in, want := range map[string]string{"10.0.0.5": "10.0.0.5:5001", "https://nas.lan:5443/": "nas.lan:5443", "[fd00::1]:5001": "[fd00::1]:5001"} {
		if got, err := normalizeAddress(in); err != nil || got != want {
			t.Errorf("%q → %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "a b", "nas.lan:0", "user@nas", "nas/x"} {
		if _, err := normalizeAddress(bad); err == nil {
			t.Errorf("%q harus ditolak", bad)
		}
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("jawaban bukan JSON: %v %s", err, rec.Body)
	}
}

// otherCert: sertifikat self-signed lain (httptest memakai satu sertifikat
// yang sama untuk semua server).
func otherCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "lain"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestRemoteBackupWizardsLoad(t *testing.T) {
	for _, id := range []string{"backup-partner-add", "backup-target-add", "backup-remote-create"} {
		if _, err := LoadWizard("../../wizards", id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestRemoteBackupFailureNotifies(t *testing.T) {
	agent := shareAgent()
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
	_ = h.s.remote.mutate(func(v *remoteFile) error {
		v.Targets = []backupTarget{{ID: "abcdef012345", Name: "Kantor"}}
		v.Jobs = []remoteJob{{ID: "kel-r", Share: "kel", Target: "abcdef012345", LastError: "NAS tujuan tidak bisa dihubungi"}}
		return nil
	})
	h.s.notifyCheck(context.Background())
	if sent.count() != 1 || !strings.Contains(sent.last().Body, "Backup kel → Kantor gagal: NAS tujuan tidak bisa dihubungi") {
		t.Fatalf("email: %d %+v", sent.count(), sent.last())
	}
}

// Label wizard tidak boleh terpotong (koma di flow mapping YAML tanpa
// tanda kutip memotong teks diam-diam).
func TestWizardLabelsComplete(t *testing.T) {
	ents, _ := os.ReadDir("../../wizards")
	for _, e := range ents {
		id := strings.TrimSuffix(e.Name(), ".yaml")
		wz, err := LoadWizard("../../wizards", id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		for _, st := range wz.Steps {
			for _, f := range st.Fields {
				for lang, v := range f.Label {
					if strings.Count(v, "(") != strings.Count(v, ")") || (lang != "id" && lang != "en") {
						t.Errorf("%s.%s: label %s terpotong: %q (%v)", id, f.Key, lang, v, f.Label)
					}
				}
			}
		}
	}
}
