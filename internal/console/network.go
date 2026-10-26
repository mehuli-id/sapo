package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// --- Reset jaringan ----------------------------------------------------------

func (m *Menu) resetNetwork(ctx context.Context) bool {
	r := m.Cfg.Recovery
	stamp := m.Now().UTC().Format("20060102-150405")
	ifBackup := r.InterfacesFile + ".sapo-" + stamp
	cfgBackup := m.AdminConfig + ".sapo-" + stamp
	m.printf("\n%s\n  %s\n  %s\n\n%s", m.t("resetNetworkHelp"), ifBackup, cfgBackup, m.t("typeReset"))
	if typed, err := m.readLine(); err != nil || typed != "RESET" {
		m.printf("%s\n", m.t("cancelled"))
		return true
	}
	ifaces, err := physicalInterfaces(r.NetDir)
	if err != nil {
		return m.fail(err)
	}
	m.printf("\n%s\n", m.t("working"))
	// sapod dihentikan dulu dan perubahan jaringan dari panel yang menunggu
	// konfirmasi dibuang, agar pengembalian otomatisnya tidak menimpa hasil
	// reset (ADR 0009).
	_ = m.systemctl(ctx, "stop", r.AgentService)
	if err := os.Remove(r.NetworkPendingFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.printf("%s\n", m.t("failed", err.Error()))
	}
	// Antarmuka diturunkan dengan pengaturan lama dulu (misalnya alamat
	// statis), baru dinaikkan lagi dengan DHCP.
	_ = m.systemctl(ctx, "stop", r.NetworkService)
	err = backupAndWrite(r.InterfacesFile, ifBackup, renderInterfaces(ifaces, wirelessIn(r.NetDir), stamp))
	if err == nil {
		err = resetAPIKeys(m.AdminConfig, cfgBackup, r.ResetAPIKeys)
	}
	m.audit("recovery.reset_network", err == nil, map[string]string{"interfaces": strings.Join(ifaces, ",")})
	// Jaringan dan sapod selalu dinyalakan lagi, juga bila langkah di atas
	// gagal.
	if startErr := m.systemctl(ctx, "start", r.NetworkService); startErr != nil && err == nil {
		err = startErr
	}
	if startErr := m.systemctl(ctx, "start", r.AgentService); startErr != nil && err == nil {
		err = startErr
	}
	if restartErr := m.systemctl(ctx, "restart", r.APIService); restartErr != nil && err == nil {
		err = restartErr
	}
	if err != nil {
		return m.fail(err)
	}
	m.printf("\n%s\n", m.t("resetNetworkDone"))
	return true
}

// ifacePattern: nama antarmuka Linux yang aman ditulis ke file ifupdown.
var ifacePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// physicalInterfaces: antarmuka jaringan fisik (punya perangkat), tanpa
// loopback, bridge, VPN, atau antarmuka container.
func physicalInterfaces(netDir string) ([]string, error) {
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !ifacePattern.MatchString(name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(netDir, name, "device")); err != nil {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	if len(out) == 0 {
		return nil, errors.New("tidak ada antarmuka jaringan fisik")
	}
	return out, nil
}

// wirelessIn: antarmuka Wi-Fi di netDir.
func wirelessIn(netDir string) func(string) bool {
	return func(name string) bool {
		for _, f := range []string{"wireless", "phy80211"} {
			if _, err := os.Stat(filepath.Join(netDir, name, f)); err == nil {
				return true
			}
		}
		return false
	}
}

// renderInterfaces menyusun /etc/network/interfaces: loopback dan DHCP
// (IPv4) serta SLAAC (IPv6) di setiap antarmuka kabel. Wi-Fi tidak dipakai
// (tanpa jaringan, DHCP-nya hanya menunda boot); disambungkan lagi lewat
// menu "Sambungkan Wi-Fi". Tanpa "source", jadi pengaturan di interfaces.d
// tidak ikut dipakai.
func renderInterfaces(ifaces []string, wireless func(string) bool, stamp string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Dibuat oleh menu darurat Sapo (reset jaringan) pada %s UTC.\n", stamp)
	b.WriteString("# File sebelumnya disimpan di samping file ini (akhiran .sapo-<waktu>).\n\n")
	b.WriteString("auto lo\niface lo inet loopback\n")
	for _, i := range ifaces {
		if wireless(i) {
			fmt.Fprintf(&b, "\n# %s (Wi-Fi) tidak dipakai.\niface %s inet manual\n", i, i)
			continue
		}
		fmt.Fprintf(&b, "\nauto %s\niface %s inet dhcp\niface %s inet6 auto\n", i, i, i)
	}
	return b.Bytes()
}

// backupAndWrite menyalin file lama ke backup (bila ada, tidak pernah
// menimpa cadangan lain), lalu menulis isi baru secara atomik. Mode dan
// pemilik file lama dipertahankan di keduanya (file bisa berisi rahasia,
// misalnya wpa-psk).
func backupAndWrite(path, backup string, data []byte) error {
	mode, uid, gid := os.FileMode(0o644), -1, -1
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mode = fi.Mode().Perm()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
		if err := writeExclusive(backup, old, mode, uid, gid); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sapo-reset-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if uid >= 0 {
		if err := tmp.Chown(uid, gid); err != nil {
			tmp.Close()
			return err
		}
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

// writeExclusive membuat file baru (O_EXCL); bila nama sudah dipakai
// (reset kedua dalam detik yang sama), akhiran -2, -3, … dipakai.
func writeExclusive(path string, data []byte, mode os.FileMode, uid, gid int) error {
	for n := 1; n < 100; n++ {
		p := path
		if n > 1 {
			p = fmt.Sprintf("%s-%d", path, n)
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Chmod(mode)
		}
		if err == nil && uid >= 0 {
			err = f.Chown(uid, gid)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	return fmt.Errorf("terlalu banyak cadangan %s", path)
}

// resetAPIKeys menghapus kunci api.<key> dari file konfigurasi admin, jadi
// nilai bawaan paket berlaku lagi. Isi lain (dan komentarnya) tetap. File
// yang tidak ada atau tanpa kunci itu tidak diubah. File yang tidak bisa
// diubah dengan aman (YAML rusak, alias, lebih dari satu dokumen) dipindah
// utuh ke cadangan: semua nilai bawaan paket berlaku lagi, karena justru
// file rusak bisa jadi penyebab panel tidak bisa dibuka.
func resetAPIKeys(path, backup string, keys []string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	moveAside := func() error {
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		uid, gid := -1, -1
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
		if err := writeExclusive(backup, data, fi.Mode().Perm(), uid, gid); err != nil {
			return err
		}
		return os.Remove(path)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return moveAside()
	}
	var extra yaml.Node
	if dec.Decode(&extra) != io.EOF || hasAlias(&doc) {
		return moveAside()
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return moveAside()
	}
	root := doc.Content[0]
	changed := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "api" || root.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		api := root.Content[i+1]
		var kept []*yaml.Node
		for j := 0; j+1 < len(api.Content); j += 2 {
			if slices.Contains(keys, api.Content[j].Value) {
				changed = true
				continue
			}
			kept = append(kept, api.Content[j], api.Content[j+1])
		}
		api.Content = kept
	}
	if !changed {
		return nil
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return backupAndWrite(path, backup, out.Bytes())
}

// hasAlias: dokumen memakai alias atau kunci gabungan (<<), yang membuat
// kunci api.* bisa datang dari tempat lain.
func hasAlias(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Value == "<<" || n.Anchor != "" {
		return true
	}
	for _, c := range n.Content {
		if hasAlias(c) {
			return true
		}
	}
	return false
}

// --- Bantuan sistem ----------------------------------------------------------

// runProgram menjalankan program (path absolut) tanpa shell.
func runProgram(ctx context.Context, prog string, args ...string) error {
	if !filepath.IsAbs(prog) {
		return fmt.Errorf("%q bukan path absolut", prog)
	}
	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return fmt.Errorf("%s %s: %v %s", filepath.Base(prog), strings.Join(args, " "), err, msg)
	}
	return nil
}

// localAddrs: alamat IPv4 lalu IPv6 global antarmuka yang menyala.
func localAddrs() []string {
	var v4, v6 []string
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || !ipn.IP.IsGlobalUnicast() {
				continue
			}
			if ipn.IP.To4() != nil {
				v4 = append(v4, ipn.IP.String())
			} else {
				v6 = append(v6, ipn.IP.String())
			}
		}
	}
	return append(v4, v6...)
}
