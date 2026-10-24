package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestTimeOps(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	prog := filepath.Join(dir, "timedatectl")
	_ = os.WriteFile(prog, []byte(`#!/bin/sh
printf '%s\n' "$*" >> '`+log+`'
case "$1" in
show) printf 'Timezone=Asia/Jakarta\nNTP=yes\nNTPSynchronized=no\n' ;;
list-timezones) printf 'Asia/Jakarta\nAsia/Makassar\nUTC\n' ;;
esac
`), 0o755)
	tt := TimeTools{Timedatectl: prog, Timeout: 5 * time.Second}
	res, err := TimeStatusOp(tt)(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st := res.(proto.TimeStatus); st.Timezone != "Asia/Jakarta" || !st.NTP || st.Synchronized {
		t.Fatalf("status = %+v", st)
	}
	set := func(tz string, ntp bool) error {
		raw, _ := json.Marshal(proto.TimeSetParams{Timezone: tz, NTP: ntp})
		_, err := TimeSetOp(tt)(context.Background(), raw)
		return err
	}
	if err := set("Asia/Makassar", false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "set-timezone -- Asia/Makassar\nset-ntp -- false") {
		t.Fatalf("panggilan:\n%s", b)
	}
	var oe *OpError
	for _, bad := range []string{"Mars/Olympus", "../../etc/passwd", "-h", ""} {
		if err := set(bad, true); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
			t.Errorf("%q harus ditolak: %v", bad, err)
		}
	}
}

func TestSharesServicesOff(t *testing.T) {
	s, _, log := testNFS(t, "0")
	s.t.SMBService = "smbd.service"
	nfs := &proto.NFSExport{Clients: []string{"10.0.0.0/8"}, User: "budi"}
	raw, _ := json.Marshal(proto.SharesApplyParams{Shares: []proto.ShareSpec{{Name: "data", Pool: "tank", Write: []string{"user:budi"}, NFS: nfs}}, SMBOff: true, NFSOff: true})
	if _, err := s.ApplyOp()(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	conf, _ := os.ReadFile(s.t.SMBConf)
	exports, _ := os.ReadFile(s.t.ExportsFile)
	if strings.Contains(string(conf), "[data]") || strings.Contains(string(exports), "/data") {
		t.Fatalf("layanan mati tidak boleh membagikan apa pun:\n%s\n%s", conf, exports)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "systemctl disable --now -- smbd.service") {
		t.Fatalf("Samba harus dimatikan:\n%s", calls)
	}
	// Dinyalakan lagi.
	raw, _ = json.Marshal(proto.SharesApplyParams{Shares: []proto.ShareSpec{{Name: "data", Pool: "tank", Write: []string{"user:budi"}}}})
	if _, err := s.ApplyOp()(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "systemctl enable --now -- smbd.service") {
		t.Fatalf("Samba harus dinyalakan:\n%s", calls)
	}
}
