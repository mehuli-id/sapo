package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	// Antarmuka diturunkan dengan pengaturan lama dulu (misalnya alamat
	// statis), baru dinaikkan lagi dengan DHCP.
	_ = m.systemctl(ctx, "stop", r.NetworkService)
	err = backupAndWrite(r.InterfacesFile, ifBackup, renderInterfaces(ifaces, stamp))
	if err == nil {
		err = resetAPIKeys(m.AdminConfig, cfgBackup, r.ResetAPIKeys)
	}
	m.audit("recovery.reset_network", err == nil, map[string]string{"interfaces": strings.Join(ifaces, ",")})
	// Jaringan selalu dinyalakan lagi, juga bila langkah di atas gagal.
	if startErr := m.systemctl(ctx, "start", r.NetworkService); startErr != nil && err == nil {
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

// renderInterfaces menyusun /etc/network/interfaces: loopback dan DHCP
// (IPv4) serta SLAAC (IPv6) di setiap antarmuka fisik. Tanpa "source",
// jadi pengaturan di interfaces.d tidak ikut dipakai.
func renderInterfaces(ifaces []string, stamp string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Dibuat oleh menu darurat Sapo (reset jaringan) pada %s UTC.\n", stamp)
	b.WriteString("# File sebelumnya disimpan di samping file ini (akhiran .sapo-<waktu>).\n\n")
	b.WriteString("auto lo\niface lo inet loopback\n")
	for _, i := range ifaces {
		fmt.Fprintf(&b, "\nauto %s\niface %s inet dhcp\niface %s inet6 auto\n", i, i, i)
	}
	return b.Bytes()
}

// backupAndWrite menyalin file lama ke backup (bila ada), lalu menulis isi
// baru secara atomik.
func backupAndWrite(path, backup string, data []byte) error {
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := os.WriteFile(backup, old, 0o644); err != nil {
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
	if err := tmp.Chmod(0o644); err != nil {
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

// resetAPIKeys menghapus kunci api.<key> dari file konfigurasi admin, jadi
// nilai bawaan paket berlaku lagi. Isi lain (dan komentarnya) tetap. File
// yang tidak ada atau tanpa kunci itu tidak diubah.
func resetAPIKeys(path, backup string, keys []string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("format %s tidak valid: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
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

func owner(path string) (int, int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("pemilik file tidak diketahui")
	}
	return int(st.Uid), int(st.Gid), nil
}
