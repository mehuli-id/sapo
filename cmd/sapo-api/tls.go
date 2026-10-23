package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/tlscert"
)

// certManager menyimpan sertifikat HTTPS yang sedang dipakai dan
// memperbaruinya tanpa restart (ADR 0004).
type certManager struct {
	opts tlscert.Options
	port string
	cfg  config.Config
	log  *slog.Logger
	cur  atomic.Pointer[tlscert.Cert]
}

func newCertManager(cfg config.Config, log *slog.Logger) (*certManager, error) {
	_, port, err := net.SplitHostPort(cfg.API.TLS.Listen)
	if err != nil {
		return nil, fmt.Errorf("api.tls.listen: %w", err)
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "sapo"
	}
	host = strings.ToLower(strings.TrimSuffix(host, ".local"))
	names := append([]string{host, host + ".local", "localhost"}, cfg.API.Hostnames...)
	ips := append([]net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, tlscert.LocalIPs()...)
	m := &certManager{
		port: port, cfg: cfg, log: log,
		opts: tlscert.Options{
			Dir:          filepath.Join(cfg.API.DataDir, "tls"),
			Organization: "Sapo",
			DNSNames:     names,
			IPs:          ips,
			Validity:     time.Duration(cfg.API.TLS.ValidityDays) * 24 * time.Hour,
			RenewBefore:  time.Duration(cfg.API.TLS.RenewBeforeDays) * 24 * time.Hour,
		},
	}
	if err := m.refresh(); err != nil {
		return nil, err
	}
	return m, nil
}

// refresh memastikan sertifikat masih layak; bila diganti, sidik jari baru
// dicatat dan layar konsol diperbarui.
func (m *certManager) refresh() error {
	c, err := tlscert.Ensure(m.opts)
	if err != nil {
		return err
	}
	m.cur.Store(&c)
	if c.Created {
		m.log.Warn("sertifikat HTTPS baru dibuat; browser akan menampilkan peringatan sekali",
			"sidik_jari_sha256", c.Fingerprint, "berlaku_sampai", c.Leaf.NotAfter.Format(time.DateOnly), "nama", c.Leaf.DNSNames)
		writeConsoleIssue(m.cfg, m, m.log)
	}
	return nil
}

func (m *certManager) renewLoop(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.refresh(); err != nil {
				m.log.Error("memperbarui sertifikat HTTPS", "err", err)
			}
		}
	}
}

func (m *certManager) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return &m.cur.Load().TLS, nil
}

func (m *certManager) fingerprint() string { return m.cur.Load().Fingerprint }

// writeConsoleIssue menulis alamat panel (dan sidik jari sertifikat) untuk
// layar login konsol NAS. /etc/issue.d/ dari paket menautkan ke file ini;
// agetty mengganti \4 dengan alamat IPv4 dan \n dengan nama host.
func writeConsoleIssue(cfg config.Config, certs *certManager, log *slog.Logger) {
	path := cfg.API.TLS.ConsoleIssueFile
	if path == "" {
		return
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return // misalnya mode pengembangan: tidak ada /run/sapo-api
	}
	var b strings.Builder
	if certs != nil {
		fmt.Fprintf(&b, "Panel Sapo: https://\\4:%s  atau  https://\\n.local:%s\n", certs.port, certs.port)
		fp := certs.fingerprint()
		half := len(fp) / 2
		// Sidik jari 95 karakter dipecah dua baris di batas byte (titik dua).
		for half < len(fp) && fp[half] != ':' {
			half++
		}
		fmt.Fprintf(&b, "Sidik jari sertifikat (SHA-256), cocokkan dengan peringatan browser:\n  %s\n  %s\n\n", fp[:half], strings.TrimPrefix(fp[half:], ":"))
	} else {
		_, port, _ := net.SplitHostPort(cfg.API.Listen)
		fmt.Fprintf(&b, "Panel Sapo: http://\\4:%s  atau  http://\\n.local:%s\n\n", port, port)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		log.Warn("menulis alamat panel untuk konsol", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Warn("menulis alamat panel untuk konsol", "err", err)
	}
}
