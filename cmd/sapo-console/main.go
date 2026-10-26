// sapo-console adalah menu darurat di konsol NAS (ADR 0008): reset password
// admin, reset jaringan, sambungkan Wi-Fi (ADR 0023), restart, dan matikan. Dijalankan systemd sebagai
// root di terminal fisik (sapo-console@tty1.service); tidak menerima
// masukan dari jaringan.
//
// "sapo-console helper …" adalah proses pembantu yang dijalankan menu
// sebagai pemilik direktori data panel, untuk membaca dan mengubah data
// akun dan log audit tanpa hak root.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"syscall"
	"time"

	"github.com/mehuli-id/sapo/internal/agent"
	"github.com/mehuli-id/sapo/internal/audit"
	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/console"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "helper" {
		os.Exit(console.HelperMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	cfgPath := flag.String("config", config.DefaultPath, "lokasi sapo.yaml milik admin")
	defaultsPath := flag.String("defaults", "", "lokasi nilai bawaan paket (misalnya "+config.DefaultsPath+")")
	flag.Parse()

	// Konfigurasi admin yang rusak tidak boleh mematikan menu: justru saat
	// itulah panel tidak bisa dibuka dan reset jaringan dibutuhkan.
	warning := ""
	cfg, err := config.LoadLayered(*defaultsPath, *cfgPath)
	if err != nil {
		warning = fmt.Sprintf("%s tidak valid (%v); menu memakai nilai bawaan. Reset jaringan memindahkan file itu ke cadangan.", *cfgPath, err)
		if cfg, err = config.LoadLayered(*defaultsPath, ""); err != nil {
			cfg = config.Default()
		}
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "sapo-console harus berjalan sebagai root di konsol NAS")
		os.Exit(1)
	}
	// Ctrl+C dan Ctrl+\ di keyboard tidak menghentikan menu; SIGTERM dari
	// systemd tetap.
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTSTP, syscall.SIGHUP)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	m := console.NewMenu(cfg, *cfgPath, os.Stdin, os.Stdout)
	m.Warning = warning
	data, err := console.NewHelperData(cfg.API.DataDir, int64(cfg.API.AuditMaxMB)<<20)
	if err != nil {
		data = unavailable{err}
	}
	m.Data = data
	// Wi-Fi lewat operasi sapod (root ke socket sapod).
	m.Agent = &agent.Client{Socket: cfg.Agent.Socket, Timeout: 2 * time.Minute}
	m.Echo = func(on bool) { setEcho(int(os.Stdin.Fd()), on) }
	// Akun debug hanya ada di ISO dev (packages-dev.list).
	if _, err := user.Lookup("user"); err == nil {
		m.DevLogin = true
	}
	if err := m.Loop(ctx); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// unavailable: data panel tidak bisa dibuka (misalnya paket baru terpasang
// dan direktori data belum ada). Menu lain tetap bisa dipakai.
type unavailable struct{ err error }

func (u unavailable) Admins() ([]string, string, error) { return nil, "", u.err }
func (u unavailable) ResetAdmin(string, string) error   { return u.err }
func (u unavailable) Audit(audit.Event)                 {}
