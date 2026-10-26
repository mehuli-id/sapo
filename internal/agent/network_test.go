package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

type netCalls struct {
	mu    sync.Mutex
	calls []string
}

func (c *netCalls) add(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, s)
}

func (c *netCalls) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.calls, "\n")
}

// testNetwork: file jaringan di direktori sementara, dua antarmuka fisik
// (eth0, eth1) dan satu virtual (docker0); program hanya dicatat.
func testNetwork(t *testing.T) (*Network, string, *netCalls) {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"net/eth0/device", "net/eth1/device", "net/docker0", "etc"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "net/eth0/address"), []byte("52:54:00:12:34:56\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "net/eth0/operstate"), []byte("up\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "net/eth0/speed"), []byte("1000\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "etc/interfaces"), []byte("# lama\nauto eth0\niface eth0 inet dhcp\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "etc/resolv.conf"), []byte("nameserver 10.0.2.3\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "etc/hosts"), []byte("127.0.0.1\tlocalhost\n127.0.1.1\tsapo\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "etc/hostname"), []byte("sapo\n"), 0o644)
	calls := &netCalls{}
	n := NewNetwork(NetworkTools{
		IP: "/usr/sbin/ip", Systemctl: "/usr/bin/systemctl", Hostnamectl: "/usr/bin/hostnamectl",
		InterfacesFile: filepath.Join(dir, "etc/interfaces"), ResolvConf: filepath.Join(dir, "etc/resolv.conf"),
		HostsFile: filepath.Join(dir, "etc/hosts"), HostnameFile: filepath.Join(dir, "etc/hostname"), NetDir: filepath.Join(dir, "net"),
		StateFile: filepath.Join(dir, "state/network.json"), PendingFile: filepath.Join(dir, "state/network-pending.json"),
		NetworkService: "networking.service", AvahiService: "avahi-daemon.service",
		ApplyDelay: 10 * time.Millisecond, ConfirmWindow: time.Hour, Timeout: time.Second,
	})
	n.run = func(_ context.Context, prog string, flags []string, operands ...string) (toolResult, error) {
		calls.add(filepath.Base(prog) + " " + strings.Join(append(flags, operands...), " "))
		switch {
		case filepath.Base(prog) == "ip" && operands[0] == "addr":
			return toolResult{Stdout: []byte(`[{"ifname":"lo","addr_info":[{"local":"127.0.0.1","prefixlen":8,"scope":"host"}]},{"ifname":"eth0","addr_info":[{"local":"10.0.2.15","prefixlen":24,"scope":"global"},{"local":"fe80::1","prefixlen":64,"scope":"link"}]}]`)}, nil
		case filepath.Base(prog) == "ip" && operands[0] == "route":
			return toolResult{Stdout: []byte(`[{"dst":"default","gateway":"10.0.2.2","dev":"eth0"}]`)}, nil
		}
		return toolResult{}, nil
	}
	return n, dir, calls
}

func call(t *testing.T, h Handler, params any) (any, error) {
	t.Helper()
	raw, _ := json.Marshal(params)
	return h(context.Background(), raw)
}

var staticCfg = proto.NetworkConfig{
	Hostname: "nas-rumah",
	Interfaces: []proto.NetworkInterface{
		{Name: "eth0", Mode: "static", Address: "192.168.1.10/24", Gateway: "192.168.1.1"},
		{Name: "eth1", Mode: "static", Address: "10.10.0.5/16"},
	},
	DNS: []string{"192.168.1.1", "2606:4700:4700::1111"},
}

func TestNetworkCheckConfig(t *testing.T) {
	n, _, _ := testNetwork(t)
	good, err := n.checkConfig(proto.NetworkConfig{Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth1", Mode: "static", Address: "192.168.5.20/24"}}}, proto.NetworkConfig{})
	if err != nil || len(good.Interfaces) != 2 || good.Interfaces[0].Mode != "dhcp" {
		t.Fatalf("antarmuka yang tidak disebut harus DHCP: %+v %v", good, err)
	}
	bad := map[string]proto.NetworkConfig{
		"nama host":             {Hostname: "NAS_1"},
		"antarmuka virtual":     {Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "docker0", Mode: "dhcp"}}},
		"baris baru di nama":    {Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0\niface", Mode: "dhcp"}}},
		"alamat jaringan":       {Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "static", Address: "192.168.1.0/24"}}},
		"alamat broadcast":      {Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "static", Address: "192.168.1.255/24"}}},
		"tanpa prefiks":         {Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "static", Address: "192.168.1.10"}}},
		"gateway di luar":       {Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "static", Address: "192.168.1.10/24", Gateway: "10.0.0.1"}}},
		"dua gateway":           {Hostname: "sapo", DNS: []string{"1.1.1.1"}, Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "static", Address: "192.168.1.10/24", Gateway: "192.168.1.1"}, {Name: "eth1", Mode: "static", Address: "10.0.0.5/24", Gateway: "10.0.0.1"}}},
		"DNS dengan DHCP":       {Hostname: "sapo", DNS: []string{"1.1.1.1"}},
		"semua tetap tanpa DNS": {Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "static", Address: "192.168.1.10/24"}, {Name: "eth1", Mode: "static", Address: "10.0.0.5/24"}}},
		"DHCP dengan alamat":    {Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "dhcp", Address: "192.168.1.10/24"}}},
		"IPv6 statis":           {Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "static", Address: "fd00::5/64"}}},
		"semua tidak dipakai":   {Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "off"}, {Name: "eth1", Mode: "off"}}},
		"DNS loopback":          {Hostname: "sapo", DNS: []string{"127.0.0.1"}, Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "static", Address: "192.168.1.10/24"}, {Name: "eth1", Mode: "off"}}},
	}
	for name, c := range bad {
		if _, err := n.checkConfig(c, proto.NetworkConfig{}); err == nil {
			t.Errorf("%s harus ditolak", name)
		}
	}
}

func TestNetworkApplyConfirm(t *testing.T) {
	n, dir, calls := testNetwork(t)
	res, err := call(t, n.ApplyOp(), proto.NetworkApplyParams{Config: staticCfg})
	if err != nil {
		t.Fatal(err)
	}
	token := res.(proto.NetworkApplyResult).Token
	// Belum berlaku: konfirmasi ditolak, perubahan kedua ditolak.
	var oe *OpError
	if _, err := call(t, n.ApplyOp(), proto.NetworkApplyParams{Config: staticCfg}); !errors.As(err, &oe) || oe.Code != proto.ErrBusy {
		t.Fatalf("apply kedua: %v", err)
	}
	waitFor(t, func() bool { return strings.Contains(calls.String(), "systemctl start networking.service") })
	ifs, _ := os.ReadFile(filepath.Join(dir, "etc/interfaces"))
	if !strings.Contains(string(ifs), "iface eth0 inet static\n    address 192.168.1.10/24\n    gateway 192.168.1.1\niface eth0 inet6 auto") ||
		!strings.Contains(string(ifs), "iface eth1 inet static\n    address 10.10.0.5/16\niface eth1 inet6 auto") {
		t.Fatalf("interfaces:\n%s", ifs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "etc/resolv.conf")); !strings.Contains(string(b), "nameserver 192.168.1.1\nnameserver 2606:4700:4700::1111\n") {
		t.Fatalf("resolv.conf:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "etc/hosts")); !strings.Contains(string(b), "127.0.1.1\tnas-rumah\n") || strings.Contains(string(b), "\tsapo") {
		t.Fatalf("hosts:\n%s", b)
	}
	inOrder(t, calls.String()+"\n", "systemctl stop networking.service", "hostnamectl set-hostname nas-rumah", "systemctl start networking.service")

	if _, err := call(t, n.ConfirmOp(), proto.NetworkTokenParams{Token: "salah"}); !errors.As(err, &oe) || oe.Code != proto.ErrNotFound {
		t.Fatalf("token salah: %v", err)
	}
	if _, err := call(t, n.ConfirmOp(), proto.NetworkTokenParams{Token: token}); err != nil {
		t.Fatal(err)
	}
	st, _ := call(t, n.StatusOp(), nil)
	status := st.(proto.NetworkStatus)
	if status.Config == nil || status.Config.Hostname != "nas-rumah" || status.Pending != nil || status.Gateway != "10.0.2.2" ||
		len(status.Interfaces) != 2 || status.Interfaces[0].MAC != "52:54:00:12:34:56" || !status.Interfaces[0].Up ||
		status.Interfaces[0].SpeedMbps != 1000 || strings.Join(status.Interfaces[0].Addresses, ",") != "10.0.2.15/24" {
		t.Fatalf("status = %+v", status)
	}
	if _, err := os.Stat(filepath.Join(dir, "state/network-pending.json")); err == nil {
		t.Fatal("catatan perubahan harus dihapus setelah konfirmasi")
	}
	// File diubah di luar Sapo (misalnya menu darurat): tidak dikelola lagi.
	_ = os.WriteFile(filepath.Join(dir, "etc/interfaces"), []byte("auto eth0\niface eth0 inet dhcp\n"), 0o644)
	st, _ = call(t, n.StatusOp(), nil)
	if st.(proto.NetworkStatus).Config != nil {
		t.Fatal("pengaturan yang diubah di luar Sapo tidak boleh tampil sebagai dikelola")
	}
}

func TestNetworkRevertsWithoutConfirmation(t *testing.T) {
	n, dir, calls := testNetwork(t)
	n.t.ConfirmWindow = 50 * time.Millisecond
	if _, err := call(t, n.ApplyOp(), proto.NetworkApplyParams{Config: staticCfg}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.pending == nil
	})
	if b, _ := os.ReadFile(filepath.Join(dir, "etc/interfaces")); string(b) != "# lama\nauto eth0\niface eth0 inet dhcp\n" {
		t.Fatalf("interfaces lama harus kembali:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "etc/resolv.conf")); string(b) != "nameserver 10.0.2.3\n" {
		t.Fatalf("resolv.conf lama harus kembali: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "etc/hosts")); !strings.Contains(string(b), "127.0.1.1\tsapo\n") {
		t.Fatalf("hosts lama harus kembali: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "state/network.json")); err == nil {
		t.Fatal("pengaturan yang tidak dikonfirmasi tidak boleh tersimpan")
	}
	if strings.Count(calls.String(), "systemctl start networking.service") != 2 {
		t.Fatalf("jaringan harus dinyalakan lagi setelah dikembalikan:\n%s", calls)
	}
}

func TestNetworkCancelAndRecover(t *testing.T) {
	n, dir, _ := testNetwork(t)
	n.t.ApplyDelay = time.Hour
	res, err := call(t, n.ApplyOp(), proto.NetworkApplyParams{Config: staticCfg})
	if err != nil {
		t.Fatal(err)
	}
	// Dibatalkan sebelum berlaku: tidak ada yang diubah.
	if _, err := call(t, n.CancelOp(), proto.NetworkTokenParams{Token: res.(proto.NetworkApplyResult).Token}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "etc/interfaces")); !strings.HasPrefix(string(b), "# lama") {
		t.Fatalf("interfaces berubah: %s", b)
	}

	// sapod berhenti setelah pengaturan baru berlaku, sebelum dikonfirmasi:
	// saat menyala lagi, pengaturan lama dikembalikan.
	n.t.ApplyDelay = 10 * time.Millisecond
	if _, err := call(t, n.ApplyOp(), proto.NetworkApplyParams{Config: staticCfg}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "etc/interfaces"))
		return strings.Contains(string(b), "static")
	})
	n.mu.Lock()
	n.timer.Stop()
	n.mu.Unlock()
	fresh := NewNetwork(n.t)
	fresh.run = n.run
	fresh.Recover(context.Background())
	if b, _ := os.ReadFile(filepath.Join(dir, "etc/interfaces")); !strings.HasPrefix(string(b), "# lama") {
		t.Fatalf("interfaces lama harus kembali setelah sapod menyala lagi:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "state/network-pending.json")); err == nil {
		t.Fatal("catatan perubahan harus dihapus")
	}
}

func TestNetworkOffAndUnmanagedFiles(t *testing.T) {
	n, dir, _ := testNetwork(t)
	cfg := proto.NetworkConfig{Hostname: "sapo", Interfaces: []proto.NetworkInterface{{Name: "eth0", Mode: "dhcp"}, {Name: "eth1", Mode: "off"}}}
	got, err := n.checkConfig(cfg, proto.NetworkConfig{})
	if err != nil {
		t.Fatal(err)
	}
	r := string(renderInterfaces(got, n.wpaConfPath))
	if strings.Contains(r, "auto eth1") || !strings.Contains(r, "iface eth1 inet manual") || !strings.Contains(r, "auto eth0\niface eth0 inet dhcp") {
		t.Fatalf("antarmuka yang tidak dipakai tidak boleh dinyalakan:\n%s", r)
	}
	// Pengaturan manual yang bisa hilang diam-diam: ditolak.
	var oe *OpError
	for name, body := range map[string]string{
		"bond":         "auto bond0\niface bond0 inet dhcp\n    bond-slaves eth0 eth1\n",
		"alamat tetap": "auto eth0\niface eth0 inet static\n    address 10.9.9.9/24\n",
		"hook":         "auto eth0\niface eth0 inet dhcp\n    post-up /root/x.sh\n",
	} {
		_ = os.WriteFile(filepath.Join(dir, "etc/interfaces"), []byte(body), 0o644)
		if _, err := call(t, n.ApplyOp(), proto.NetworkApplyParams{Config: cfg}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
			t.Errorf("%s: %v", name, err)
		}
	}
	// interfaces.d yang berisi file juga; yang kosong boleh.
	_ = os.MkdirAll(filepath.Join(dir, "etc/interfaces.d"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "etc/interfaces"), []byte("source interfaces.d/*\nauto lo\niface lo inet loopback\nallow-hotplug eth0\niface eth0 inet dhcp\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "etc/interfaces.d/vlan"), []byte("auto eth0.10\n"), 0o644)
	if _, err := call(t, n.ApplyOp(), proto.NetworkApplyParams{Config: cfg}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Errorf("interfaces.d berisi: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "etc/interfaces.d/vlan"))
	n.t.ApplyDelay = time.Hour
	if _, err := call(t, n.ApplyOp(), proto.NetworkApplyParams{Config: cfg}); err != nil {
		t.Fatalf("pengaturan dasar boleh diganti: %v", err)
	}
	// resolv.conf dan hosts tidak disentuh bila DNS dan nama host tidak berubah.
	n.mu.Lock()
	_, hasResolv := n.pending.Old[n.t.ResolvConf]
	_, hasHosts := n.pending.Old[n.t.HostsFile]
	n.timer.Stop()
	n.mu.Unlock()
	if hasResolv || hasHosts {
		t.Fatal("file yang tidak diubah tidak boleh disimpan atau dikembalikan")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "state/interfaces.before-sapo-*")); len(m) != 1 {
		t.Fatalf("cadangan pengaturan sebelum Sapo: %v", m)
	}
}

func TestNetworkRefusesSymlinkedResolvConf(t *testing.T) {
	n, dir, _ := testNetwork(t)
	_ = os.Remove(filepath.Join(dir, "etc/resolv.conf"))
	_ = os.Symlink("/run/systemd/resolve/stub-resolv.conf", filepath.Join(dir, "etc/resolv.conf"))
	var oe *OpError
	if _, err := call(t, n.ApplyOp(), proto.NetworkApplyParams{Config: staticCfg}); !errors.As(err, &oe) || oe.Code != proto.ErrConflict {
		t.Fatalf("resolv.conf symlink: %v", err)
	}
}
