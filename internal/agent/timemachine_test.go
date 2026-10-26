package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestSharesTimeMachine(t *testing.T) {
	s, _, log := testShares(t, "0")
	spec := proto.ShareSpec{Name: "mac", Pool: "tank", Write: []string{"user:budi"}, RecycleDays: 30, TimeMachine: &proto.TimeMachine{QuotaGB: 500}}
	res, err := applyShares(t, s, spec, proto.ShareSpec{Name: "keluarga", Pool: "tank", Write: []string{"user:budi"}, RecycleDays: 30})
	if err != nil || res.Shares[0].TimeMachineError != "" {
		t.Fatalf("%v %+v", err, res)
	}
	props, _ := s.zfsProps(context.Background(), "tank/mac", tmPropName, "refquota", "xattr", recyclePropName)
	if props["refquota"] != "536870912000" || props[tmPropName] != "500" || props["xattr"] != "sa" || recycleDays(props[recyclePropName]) != 0 {
		t.Fatalf("properti = %+v", props)
	}
	conf, _ := os.ReadFile(s.t.SMBConf)
	c := string(conf)
	mac := c[strings.Index(c, "[mac]"):]
	if i := strings.Index(mac[1:], "\n["); i > 0 {
		mac = mac[:i+1]
	}
	for _, want := range []string{"vfs objects = catia fruit streams_xattr shadow_copy2", "fruit:time machine = yes", "fruit:time machine max size = 500G", "durable handles = yes"} {
		if !strings.Contains(mac, want) {
			t.Errorf("[mac] tanpa %q:\n%s", want, mac)
		}
	}
	// Tempat sampah tidak pernah di folder Time Machine; folder lain tetap.
	if strings.Contains(mac, "recycle") || !strings.Contains(c, "recycle:repository") || strings.Count(c, "fruit:time machine = yes") != 1 {
		t.Fatalf("smb.conf:\n%s", c)
	}
	if !strings.Contains(c, "multicast dns register = yes") {
		t.Fatal("Samba harus mengumumkan folder Time Machine lewat Avahi")
	}
	// Dimatikan: kuota buatan Sapo dilepas.
	spec.TimeMachine = nil
	if _, err := applyShares(t, s, spec); err != nil {
		t.Fatal(err)
	}
	props, _ = s.zfsProps(context.Background(), "tank/mac", tmPropName, "refquota")
	if props["refquota"] != "none" || props[tmPropName] != "off" {
		t.Fatalf("mati: %+v", props)
	}
	// Folder yang tidak pernah jadi Time Machine: kuotanya tidak disentuh.
	os.Remove(log)
	if _, err := applyShares(t, s, proto.ShareSpec{Name: "keluarga", Pool: "tank", Write: []string{"user:budi"}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "refquota=") {
		t.Fatalf("kuota folder biasa tidak boleh diubah:\n%s", b)
	}
	if err := s.setTimeMachine(context.Background(), "tank/mac", &proto.TimeMachine{QuotaGB: 1}); err == nil {
		t.Fatal("kuota terlalu kecil harus ditolak")
	}
}
