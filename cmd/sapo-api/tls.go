package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mehuli-id/sapo/internal/api"
	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/proto"
	"github.com/mehuli-id/sapo/internal/tlscert"
)

// certManager menyimpan sertifikat HTTPS yang sedang dipakai dan
// memperbaruinya tanpa restart (ADR 0004).
type certManager struct {
	opts    tlscert.Options
	port    string
	cfg     config.Config
	log     *slog.Logger
	cur     atomic.Pointer[tlscert.Cert]
	console func(scheme string, port string, fingerprint string)
}

func newCertManager(cfg config.Config, log *slog.Logger, console func(scheme, port, fingerprint string)) (*certManager, error) {
	_, port, err := net.SplitHostPort(cfg.API.TLS.Listen)
	if err != nil {
		return nil, fmt.Errorf("api.tls.listen: %w", err)
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "sapo"
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".local")
	names := append([]string{host, host + ".local", "localhost"}, cfg.API.Hostnames...)
	ips := append([]net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, tlscert.LocalIPs()...)
	m := &certManager{
		port: port, cfg: cfg, log: log, console: console,
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
		m.console("https", m.port, c.Fingerprint)
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

// announceConsole meminta sapod memperbarui alamat panel (dan sidik jari) di
// layar login konsol NAS (operasi console.panel_address). sapo-api hanya
// mengirim skema, port, dan sidik jari; isi layar dibuat sapod dari templat
// tetap. sapod mungkin belum siap saat sapo-api baru menyala, jadi dicoba
// ulang beberapa kali.
func announceConsole(ctx context.Context, caller api.AgentCaller, log *slog.Logger, scheme, port, fingerprint string) {
	n, err := strconv.Atoi(port)
	if err != nil {
		return
	}
	params := proto.ConsolePanelParams{Scheme: scheme, Port: n, Fingerprint: fingerprint}
	for attempt := 0; attempt < 10; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = caller.Call(callCtx, proto.OpConsolePanelAddress, params, nil)
		cancel()
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
	log.Warn("alamat panel tidak bisa ditampilkan di layar konsol", "err", err)
}
