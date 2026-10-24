package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// NetworkTools mengatur operasi network.* (agent.network di sapo.yaml,
// ADR 0009). /etc/network/interfaces (ifupdown) ditulis utuh oleh sapod dari
// templat tetap; setiap perubahan harus dikonfirmasi dalam ConfirmWindow,
// bila tidak pengaturan lama dikembalikan.
type NetworkTools struct {
	IP, Systemctl, Hostnamectl string
	InterfacesFile             string
	ResolvConf, HostsFile      string
	NetDir                     string
	// StateFile: pengaturan yang terakhir dikonfirmasi; PendingFile:
	// salinan file lama selama menunggu konfirmasi (dipakai juga untuk
	// mengembalikan saat sapod menyala lagi).
	StateFile, PendingFile       string
	NetworkService, AvahiService string
	ApplyDelay, ConfirmWindow    time.Duration
	Timeout                      time.Duration
	Log                          *slog.Logger
}

// Network melayani network.status, apply, confirm, dan cancel.
type Network struct {
	t   NetworkTools
	now func() time.Time
	// run dan setHostname diganti di test.
	run func(ctx context.Context, prog string, flags []string, operands ...string) (toolResult, error)

	mu      sync.Mutex
	pending *networkPending
	timer   *time.Timer
}

// networkPending disimpan di PendingFile: berkas lama dan pengaturan baru.
type networkPending struct {
	TokenHash  string              `json:"token_hash"`
	ConfirmBy  time.Time           `json:"confirm_by"`
	Config     proto.NetworkConfig `json:"config"`
	Old        map[string]*string  `json:"old"` // path → isi lama (nil = tidak ada)
	OldMode    map[string]uint32   `json:"old_mode"`
	OldHost    string              `json:"old_hostname"`
	Applied    bool                `json:"applied"`
	ApplyError string              `json:"apply_error,omitempty"`
}

// networkState disimpan di StateFile.
type networkState struct {
	Config proto.NetworkConfig `json:"config"`
	// Hash isi InterfacesFile yang ditulis sapod: bila berbeda (diubah di
	// luar Sapo, misalnya reset dari menu darurat), pengaturan dianggap
	// tidak dikelola Sapo lagi.
	Hash string `json:"hash"`
}

// NewNetwork menyiapkan operasi jaringan. Perubahan yang belum dikonfirmasi
// saat sapod berhenti (misalnya NAS dimatikan) dikembalikan sekarang.
func NewNetwork(t NetworkTools) *Network {
	n := &Network{t: t, now: time.Now}
	n.run = func(ctx context.Context, prog string, flags []string, operands ...string) (toolResult, error) {
		return runTool(ctx, n.t.Timeout, prog, flags, operands...)
	}
	return n
}

// Recover mengembalikan perubahan yang tidak sempat dikonfirmasi.
func (n *Network) Recover(ctx context.Context) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending != nil {
		return // perubahan baru dari sapod ini, bukan sisa sebelumnya
	}
	data, err := os.ReadFile(n.t.PendingFile)
	if err != nil {
		return
	}
	var p networkPending
	if err := json.Unmarshal(data, &p); err != nil {
		n.logWarn("catatan perubahan jaringan rusak; dihapus", "err", err)
		_ = os.Remove(n.t.PendingFile)
		return
	}
	n.logWarn("perubahan jaringan belum dikonfirmasi saat sapod berhenti; pengaturan lama dikembalikan")
	n.revertLocked(ctx, &p)
}

func (n *Network) logWarn(msg string, args ...any) {
	if n.t.Log != nil {
		n.t.Log.Warn(msg, args...)
	}
}

func (n *Network) logInfo(msg string, args ...any) {
	if n.t.Log != nil {
		n.t.Log.Info(msg, args...)
	}
}

// --- Validasi dan render -----------------------------------------------------

var (
	hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	ifacePattern    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
)

// physicalInterfaces: antarmuka jaringan fisik (punya perangkat), urut nama.
func (n *Network) physicalInterfaces() ([]string, error) {
	entries, err := os.ReadDir(n.t.NetDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !ifacePattern.MatchString(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(n.t.NetDir, e.Name(), "device")); err == nil {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out, nil
}

// checkConfig memeriksa dan menyamakan pengaturan: setiap antarmuka fisik
// ada tepat sekali (yang tidak disebut memakai DHCP), alamat tetap IPv4 yang
// sah, paling banyak satu gateway, dan DNS hanya (dan wajib) bila semua
// antarmuka beralamat tetap.
func (n *Network) checkConfig(c proto.NetworkConfig) (proto.NetworkConfig, error) {
	if !hostnamePattern.MatchString(c.Hostname) || c.Hostname == "localhost" {
		return c, errors.New("nama host hanya huruf kecil, angka, dan tanda hubung (1-63 karakter)")
	}
	phys, err := n.physicalInterfaces()
	if err != nil {
		return c, err
	}
	if len(phys) == 0 {
		return c, errors.New("tidak ada antarmuka jaringan fisik")
	}
	byName := map[string]proto.NetworkInterface{}
	for _, i := range c.Interfaces {
		if !slices.Contains(phys, i.Name) {
			return c, fmt.Errorf("antarmuka %s tidak ada", clip(i.Name))
		}
		if _, dup := byName[i.Name]; dup {
			return c, fmt.Errorf("antarmuka %s disebut dua kali", i.Name)
		}
		byName[i.Name] = i
	}
	out := proto.NetworkConfig{Hostname: c.Hostname, DNS: []string{}}
	gateways, dhcp := 0, false
	for _, name := range phys {
		i, ok := byName[name]
		if !ok {
			i = proto.NetworkInterface{Name: name, Mode: "dhcp"}
		}
		switch i.Mode {
		case "dhcp":
			if i.Address != "" || i.Gateway != "" {
				return c, fmt.Errorf("%s: DHCP tidak memakai alamat atau gateway tetap", name)
			}
			dhcp = true
		case "static":
			p, err := netip.ParsePrefix(i.Address)
			if err != nil || !p.Addr().Is4() || p.Bits() < 8 || p.Bits() > 30 {
				return c, fmt.Errorf("%s: alamat harus IPv4 dengan prefiks /8–/30, misalnya 192.168.1.10/24", name)
			}
			a := p.Addr()
			if !a.IsGlobalUnicast() || a == p.Masked().Addr() || a == lastAddr(p) {
				return c, fmt.Errorf("%s: %s tidak bisa dipakai sebagai alamat NAS", name, a)
			}
			i.Address = p.String()
			if i.Gateway != "" {
				g, err := netip.ParseAddr(i.Gateway)
				if err != nil || !p.Contains(g) || g == a || g == p.Masked().Addr() || g == lastAddr(p) {
					return c, fmt.Errorf("%s: gateway harus alamat lain di jaringan %s", name, p.Masked())
				}
				i.Gateway = g.String()
				gateways++
			}
		default:
			return c, fmt.Errorf("%s: mode harus dhcp atau static", name)
		}
		out.Interfaces = append(out.Interfaces, i)
	}
	if gateways > 1 {
		return c, errors.New("gateway hanya boleh di satu antarmuka")
	}
	if dhcp && len(c.DNS) > 0 {
		return c, errors.New("DNS diatur DHCP selama ada antarmuka DHCP; kosongkan DNS")
	}
	if !dhcp && (len(c.DNS) == 0 || len(c.DNS) > 3) {
		return c, errors.New("isi 1–3 server DNS bila semua antarmuka beralamat tetap")
	}
	for _, d := range c.DNS {
		a, err := netip.ParseAddr(d)
		if err != nil || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() {
			return c, fmt.Errorf("server DNS tidak valid: %s", clip(d))
		}
		out.DNS = append(out.DNS, a.String())
	}
	return out, nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// renderInterfaces menyusun /etc/network/interfaces dari pengaturan yang
// sudah diperiksa (nama antarmuka dan alamat tidak bisa menambah baris).
func renderInterfaces(c proto.NetworkConfig) []byte {
	var b bytes.Buffer
	b.WriteString("# Dibuat oleh sapod (Sapo). Jangan diubah manual: atur jaringan lewat panel.\n")
	b.WriteString("# Menu darurat di konsol NAS bisa mengembalikan semua antarmuka ke DHCP.\n\n")
	b.WriteString("auto lo\niface lo inet loopback\n")
	for _, i := range c.Interfaces {
		fmt.Fprintf(&b, "\nauto %s\n", i.Name)
		if i.Mode == "static" {
			fmt.Fprintf(&b, "iface %s inet static\n    address %s\n", i.Name, i.Address)
			if i.Gateway != "" {
				fmt.Fprintf(&b, "    gateway %s\n", i.Gateway)
			}
		} else {
			fmt.Fprintf(&b, "iface %s inet dhcp\n", i.Name)
		}
		fmt.Fprintf(&b, "iface %s inet6 auto\n", i.Name)
	}
	return b.Bytes()
}

func renderResolvConf(dns []string) []byte {
	var b bytes.Buffer
	b.WriteString("# Dibuat oleh sapod (Sapo): server DNS dari pengaturan jaringan di panel.\n")
	for _, d := range dns {
		fmt.Fprintf(&b, "nameserver %s\n", d)
	}
	return b.Bytes()
}

// renderHosts mengganti baris 127.0.1.1 (nama host mesin) di /etc/hosts.
func renderHosts(old []byte, hostname string) []byte {
	var b bytes.Buffer
	done := false
	sc := bufio.NewScanner(bytes.NewReader(old))
	for sc.Scan() {
		line := sc.Text()
		if f := strings.Fields(line); len(f) > 0 && f[0] == "127.0.1.1" {
			if !done {
				fmt.Fprintf(&b, "127.0.1.1\t%s\n", hostname)
				done = true
			}
			continue
		}
		b.WriteString(line + "\n")
	}
	if !done {
		fmt.Fprintf(&b, "127.0.1.1\t%s\n", hostname)
	}
	return b.Bytes()
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- Operasi -----------------------------------------------------------------

// StatusOp mengembalikan handler network.status.
func (n *Network) StatusOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		out := proto.NetworkStatus{Interfaces: []proto.NetworkLink{}, DNS: []string{}}
		out.Hostname, _ = os.Hostname()
		if cfg, ok := n.managedConfig(); ok {
			out.Config = &cfg
		}
		n.mu.Lock()
		if n.pending != nil {
			out.Pending = &proto.NetworkPending{ConfirmBy: n.pending.ConfirmBy, Config: n.pending.Config}
		}
		n.mu.Unlock()
		phys, err := n.physicalInterfaces()
		if err != nil {
			return nil, err
		}
		addrs := n.addresses(ctx)
		for _, name := range phys {
			l := proto.NetworkLink{Name: name, Addresses: addrs[name]}
			if l.Addresses == nil {
				l.Addresses = []string{}
			}
			dir := filepath.Join(n.t.NetDir, name)
			l.MAC = strings.TrimSpace(readSmall(filepath.Join(dir, "address")))
			l.Up = strings.TrimSpace(readSmall(filepath.Join(dir, "operstate"))) == "up"
			if v, err := strconv.Atoi(strings.TrimSpace(readSmall(filepath.Join(dir, "speed")))); err == nil && v > 0 {
				l.SpeedMbps = v
			}
			out.Interfaces = append(out.Interfaces, l)
		}
		out.Gateway = n.defaultGateway(ctx)
		if data, err := os.ReadFile(n.t.ResolvConf); err == nil {
			sc := bufio.NewScanner(bytes.NewReader(data))
			for sc.Scan() {
				if f := strings.Fields(sc.Text()); len(f) == 2 && f[0] == "nameserver" {
					if a, err := netip.ParseAddr(f[1]); err == nil {
						out.DNS = append(out.DNS, a.String())
					}
				}
			}
		}
		return out, nil
	}
}

func readSmall(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 256)
	k, _ := f.Read(buf)
	return string(buf[:k])
}

// addresses: alamat (dengan prefiks) setiap antarmuka dari "ip -j addr".
func (n *Network) addresses(ctx context.Context) map[string][]string {
	// ip membaca "--" sebagai akhir opsi umum; perintahnya operand.
	res, err := n.run(ctx, n.t.IP, []string{"-j"}, "addr", "show")
	out := map[string][]string{}
	if err != nil || res.ExitCode != 0 {
		return out
	}
	var links []struct {
		Ifname   string `json:"ifname"`
		AddrInfo []struct {
			Local     string `json:"local"`
			Prefixlen int    `json:"prefixlen"`
			Scope     string `json:"scope"`
		} `json:"addr_info"`
	}
	if json.Unmarshal(res.Stdout, &links) != nil {
		return out
	}
	for _, l := range links {
		for _, a := range l.AddrInfo {
			if ip, err := netip.ParseAddr(a.Local); err == nil && a.Scope == "global" {
				out[l.Ifname] = append(out[l.Ifname], netip.PrefixFrom(ip, a.Prefixlen).String())
			}
		}
	}
	return out
}

func (n *Network) defaultGateway(ctx context.Context) string {
	res, err := n.run(ctx, n.t.IP, []string{"-j", "-4"}, "route", "show", "default")
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	var routes []struct {
		Gateway string `json:"gateway"`
	}
	if json.Unmarshal(res.Stdout, &routes) != nil || len(routes) == 0 {
		return ""
	}
	if a, err := netip.ParseAddr(routes[0].Gateway); err == nil {
		return a.String()
	}
	return ""
}

// managedConfig: pengaturan terakhir yang dikonfirmasi, bila file jaringan
// masih persis yang ditulis sapod.
func (n *Network) managedConfig() (proto.NetworkConfig, bool) {
	data, err := os.ReadFile(n.t.StateFile)
	if err != nil {
		return proto.NetworkConfig{}, false
	}
	var st networkState
	if json.Unmarshal(data, &st) != nil {
		return proto.NetworkConfig{}, false
	}
	cur, err := os.ReadFile(n.t.InterfacesFile)
	if err != nil || hashBytes(cur) != st.Hash {
		return proto.NetworkConfig{}, false
	}
	return st.Config, true
}

// ApplyOp mengembalikan handler network.apply. Pengaturan baru berlaku
// setelah ApplyDelay (agar jawaban sampai ke browser sebelum alamat
// berubah) dan harus dikonfirmasi sebelum ConfirmBy.
func (n *Network) ApplyOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.NetworkApplyParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		cfg, err := n.checkConfig(req.Config)
		if err != nil {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		if fi, err := os.Lstat(n.t.ResolvConf); err == nil && fi.Mode()&fs.ModeSymlink != 0 && len(cfg.DNS) > 0 {
			return nil, &OpError{Code: proto.ErrConflict, Message: n.t.ResolvConf + " dikelola program lain (symlink); DNS tidak bisa diatur"}
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.pending != nil {
			return nil, &OpError{Code: proto.ErrBusy, Message: "perubahan jaringan sebelumnya belum dikonfirmasi"}
		}
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			return nil, err
		}
		token := hex.EncodeToString(tokenBytes)
		now := n.now()
		p := &networkPending{
			TokenHash: hashBytes([]byte(token)),
			ConfirmBy: now.Add(n.t.ApplyDelay + n.t.ConfirmWindow),
			Config:    cfg,
			Old:       map[string]*string{},
			OldMode:   map[string]uint32{},
		}
		p.OldHost, _ = os.Hostname()
		for _, path := range []string{n.t.InterfacesFile, n.t.ResolvConf, n.t.HostsFile, n.t.StateFile} {
			data, err := os.ReadFile(path)
			switch {
			case err == nil:
				s := string(data)
				p.Old[path] = &s
				if fi, err := os.Stat(path); err == nil {
					p.OldMode[path] = uint32(fi.Mode().Perm())
				}
			case errors.Is(err, fs.ErrNotExist):
				p.Old[path] = nil
			default:
				return nil, err
			}
		}
		// Salinan lama disimpan dulu: bila sapod atau NAS berhenti sebelum
		// konfirmasi, pengaturan lama dikembalikan saat menyala lagi.
		if err := n.savePending(p); err != nil {
			return nil, err
		}
		n.pending = p
		n.timer = time.AfterFunc(n.t.ApplyDelay, func() { n.applyPending(p) })
		n.logInfo("pengaturan jaringan baru dijadwalkan", "hostname", cfg.Hostname, "konfirmasi_sebelum", p.ConfirmBy)
		return proto.NetworkApplyResult{Token: token, AppliesAt: now.Add(n.t.ApplyDelay), ConfirmBy: p.ConfirmBy}, nil
	}
}

func (n *Network) savePending(p *networkPending) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writePrivateFile(n.t.PendingFile, data)
}

// writePrivateFile menulis file 0600 secara atomik (direktori dibuat 0700).
func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// applyPending menerapkan pengaturan baru (berjalan di latar belakang),
// lalu menjadwalkan pengembalian bila tidak dikonfirmasi.
func (n *Network) applyPending(p *networkPending) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pending != p {
		return // sudah dibatalkan
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err := n.writeConfig(ctx, p.Config)
	p.Applied = true
	if err != nil {
		p.ApplyError = err.Error()
		n.logWarn("pengaturan jaringan gagal diterapkan; dikembalikan", "err", err)
		n.revertLocked(ctx, p)
		return
	}
	_ = n.savePending(p)
	wait := time.Until(p.ConfirmBy)
	n.timer = time.AfterFunc(wait, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.pending != p {
			return
		}
		n.logWarn("pengaturan jaringan tidak dikonfirmasi; pengaturan lama dikembalikan")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		n.revertLocked(ctx, p)
	})
}

// writeConfig menurunkan jaringan dengan pengaturan lama, menulis file
// baru, mengganti nama host, lalu menaikkan jaringan lagi.
func (n *Network) writeConfig(ctx context.Context, c proto.NetworkConfig) error {
	n.networkDown(ctx)
	var errs []error
	errs = append(errs, writeRootFile(n.t.InterfacesFile, renderInterfaces(c)))
	if len(c.DNS) > 0 {
		errs = append(errs, writeRootFile(n.t.ResolvConf, renderResolvConf(c.DNS)))
	}
	errs = append(errs, n.setHostname(ctx, c.Hostname))
	errs = append(errs, n.service(ctx, "start", n.t.NetworkService))
	_ = n.service(ctx, "restart", n.t.AvahiService)
	return errors.Join(errs...)
}

func (n *Network) setHostname(ctx context.Context, name string) error {
	if cur, _ := os.Hostname(); cur == name {
		return nil
	}
	old, _ := os.ReadFile(n.t.HostsFile)
	if err := writeRootFile(n.t.HostsFile, renderHosts(old, name)); err != nil {
		return err
	}
	res, err := n.run(ctx, n.t.Hostnamectl, []string{"set-hostname"}, name)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("hostnamectl: %s", firstLine(res.Stderr))
	}
	return nil
}

// networkDown menurunkan jaringan dengan pengaturan yang sedang berlaku,
// lalu membersihkan alamat IPv4 yang tersisa di antarmuka fisik (misalnya
// dari DHCP di luar ifupdown), agar alamat baru bisa dipasang.
func (n *Network) networkDown(ctx context.Context) {
	_ = n.service(ctx, "stop", n.t.NetworkService)
	phys, _ := n.physicalInterfaces()
	for _, name := range phys {
		_, _ = n.run(ctx, n.t.IP, []string{"-4"}, "addr", "flush", "dev", name, "scope", "global")
	}
}

func (n *Network) service(ctx context.Context, action, unit string) error {
	res, err := n.run(ctx, n.t.Systemctl, []string{action}, unit)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("systemctl %s %s: %s", action, unit, firstLine(res.Stderr))
	}
	return nil
}

// revertLocked mengembalikan semua file lama dan nama host, lalu menaikkan
// jaringan lagi. n.mu dipegang pemanggil.
func (n *Network) revertLocked(ctx context.Context, p *networkPending) {
	if n.timer != nil {
		n.timer.Stop()
	}
	n.networkDown(ctx)
	for path, old := range p.Old {
		var err error
		if old == nil {
			err = os.Remove(path)
			if errors.Is(err, fs.ErrNotExist) {
				err = nil
			}
		} else if path == n.t.StateFile {
			err = writePrivateFile(path, []byte(*old))
		} else {
			err = writeRootFile(path, []byte(*old))
			if mode, ok := p.OldMode[path]; err == nil && ok {
				err = os.Chmod(path, fs.FileMode(mode))
			}
		}
		if err != nil {
			n.logWarn("file jaringan lama gagal dikembalikan", "file", path, "err", err)
		}
	}
	if p.OldHost != "" {
		if cur, _ := os.Hostname(); cur != p.OldHost {
			if res, err := n.run(ctx, n.t.Hostnamectl, []string{"set-hostname"}, p.OldHost); err != nil || res.ExitCode != 0 {
				n.logWarn("nama host lama gagal dikembalikan", "err", err)
			}
		}
	}
	if err := n.service(ctx, "start", n.t.NetworkService); err != nil {
		n.logWarn("jaringan gagal dinyalakan lagi", "err", err)
	}
	_ = n.service(ctx, "restart", n.t.AvahiService)
	_ = os.Remove(n.t.PendingFile)
	n.pending, n.timer = nil, nil
	n.logInfo("pengaturan jaringan lama dikembalikan")
}

// checkToken: perubahan yang menunggu dan token yang cocok. n.mu dipegang.
func (n *Network) checkToken(raw json.RawMessage) (*networkPending, error) {
	var req proto.NetworkTokenParams
	if err := decodeParams(raw, &req); err != nil {
		return nil, err
	}
	p := n.pending
	if p == nil || subtle.ConstantTimeCompare([]byte(hashBytes([]byte(req.Token))), []byte(p.TokenHash)) != 1 {
		return nil, &OpError{Code: proto.ErrNotFound, Message: "tidak ada perubahan jaringan dengan token ini"}
	}
	return p, nil
}

// ConfirmOp mengembalikan handler network.confirm: pengaturan baru disimpan.
func (n *Network) ConfirmOp() Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		p, err := n.checkToken(raw)
		if err != nil {
			return nil, err
		}
		if !p.Applied {
			return nil, &OpError{Code: proto.ErrBusy, Message: "pengaturan baru belum mulai berlaku"}
		}
		cur, err := os.ReadFile(n.t.InterfacesFile)
		if err != nil {
			return nil, err
		}
		st, _ := json.Marshal(networkState{Config: p.Config, Hash: hashBytes(cur)})
		if err := writePrivateFile(n.t.StateFile, st); err != nil {
			return nil, err
		}
		if n.timer != nil {
			n.timer.Stop()
		}
		_ = os.Remove(n.t.PendingFile)
		n.pending, n.timer = nil, nil
		n.logInfo("pengaturan jaringan dikonfirmasi", "hostname", p.Config.Hostname)
		return map[string]bool{"ok": true}, nil
	}
}

// CancelOp mengembalikan handler network.cancel: pengaturan lama langsung
// dikembalikan.
func (n *Network) CancelOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		p, err := n.checkToken(raw)
		if err != nil {
			return nil, err
		}
		if !p.Applied {
			// Belum ada yang diubah: cukup batalkan jadwalnya.
			if n.timer != nil {
				n.timer.Stop()
			}
			_ = os.Remove(n.t.PendingFile)
			n.pending, n.timer = nil, nil
			return map[string]bool{"ok": true}, nil
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		n.revertLocked(ctx, p)
		return map[string]bool{"ok": true}, nil
	}
}
