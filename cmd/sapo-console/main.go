// sapo-console adalah menu darurat di konsol NAS (ADR 0008): reset password
// admin, reset jaringan, restart, dan matikan. Dijalankan systemd sebagai
// root di terminal fisik (sapo-console@tty1.service); tidak menerima
// masukan dari jaringan.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/console"
)

func main() {
	cfgPath := flag.String("config", config.DefaultPath, "lokasi sapo.yaml milik admin")
	defaultsPath := flag.String("defaults", "", "lokasi nilai bawaan paket (misalnya "+config.DefaultsPath+")")
	flag.Parse()

	cfg, err := config.LoadLayered(*defaultsPath, *cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "konfigurasi:", err)
		os.Exit(1)
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
	if err := m.Loop(ctx); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
