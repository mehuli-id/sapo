// Package console adalah menu darurat di konsol NAS (sapo-console, ADR
// 0008): reset password admin, reset jaringan, restart, dan matikan. Menu
// hanya membaca keyboard dan menulis ke layar yang terpasang ke NAS; tidak
// ada masukan dari jaringan.
package console

import (
	"bufio"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/mehuli-id/sapo/internal/audit"
	"github.com/mehuli-id/sapo/internal/auth"
	"github.com/mehuli-id/sapo/internal/config"
)

//go:embed messages.json
var messagesJSON []byte

// Menu adalah satu sesi menu di satu terminal.
type Menu struct {
	Cfg config.Config
	// AdminConfig: file konfigurasi admin (/etc/sapo/sapo.yaml).
	AdminConfig string
	In          *bufio.Reader
	Out         io.Writer
	// Refresh: layar menu digambar ulang setiap selang ini saat menunggu
	// pilihan (alamat IP dan sidik jari bisa berubah); 0 = tidak.
	Refresh time.Duration
	// Run menjalankan program tanpa shell (systemctl).
	Run      func(ctx context.Context, prog string, args ...string) error
	Hostname func() (string, error)
	// Addrs: alamat IP NAS untuk ditampilkan.
	Addrs func() []string
	Now   func() time.Time

	msg   map[string]string
	lines chan lineResult
}

type lineResult struct {
	line string
	err  error
}

// NewMenu menyiapkan menu dengan bahasa setup awal (bawaan Indonesia).
func NewMenu(cfg config.Config, adminConfig string, in io.Reader, out io.Writer) *Menu {
	m := &Menu{Cfg: cfg, AdminConfig: adminConfig, In: bufio.NewReader(in), Out: out, Run: runProgram,
		Hostname: os.Hostname, Addrs: localAddrs, Now: time.Now, Refresh: 30 * time.Second}
	m.setLanguage("id")
	return m
}

func (m *Menu) setLanguage(lang string) {
	var all map[string]map[string]string
	if err := json.Unmarshal(messagesJSON, &all); err != nil {
		panic(err)
	}
	if msg, ok := all[lang]; ok {
		m.msg = msg
	} else {
		m.msg = all["id"]
	}
}

func (m *Menu) t(key string, args ...any) string {
	s, ok := m.msg[key]
	if !ok {
		return key
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

func (m *Menu) printf(format string, args ...any) { fmt.Fprintf(m.Out, format, args...) }

// readLine membaca satu baris dari keyboard. io.EOF bila terminal ditutup.
func (m *Menu) readLine() (string, error) {
	r := <-m.lines
	return r.line, r.err
}

// readLines membaca keyboard di latar belakang, agar menu bisa digambar
// ulang sambil menunggu.
func (m *Menu) readLines() {
	for {
		line, err := m.In.ReadString('\n')
		if err != nil && line == "" {
			m.lines <- lineResult{err: err}
			return
		}
		m.lines <- lineResult{line: strings.TrimSpace(line)}
	}
}

// readChoice menunggu pilihan menu; ok = false bila waktunya menggambar
// ulang layar.
func (m *Menu) readChoice(ctx context.Context) (string, bool, error) {
	var tick <-chan time.Time
	if m.Refresh > 0 {
		t := time.NewTimer(m.Refresh)
		defer t.Stop()
		tick = t.C
	}
	select {
	case r := <-m.lines:
		return r.line, true, r.err
	case <-tick:
		return "", false, nil
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

// Loop menampilkan menu sampai terminal ditutup.
func (m *Menu) Loop(ctx context.Context) error {
	if st, err := auth.OpenStore(m.Cfg.API.DataDir); err == nil {
		m.setLanguage(st.Language())
	}
	m.lines = make(chan lineResult)
	go m.readLines()
	for ctx.Err() == nil {
		m.screen()
		choice, ok, err := m.readChoice(ctx)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		var done bool
		switch choice {
		case "1":
			done = m.resetAdmin(ctx)
		case "2":
			done = m.resetNetwork(ctx)
		case "3":
			done = m.power(ctx, "reboot", "confirmReboot")
		case "4":
			done = m.power(ctx, "poweroff", "confirmPoweroff")
		case "5", "":
			continue
		default:
			m.printf("%s\n", m.t("invalid"))
			done = true
		}
		if done {
			m.printf("\n%s\n", m.t("pressEnter"))
			if _, err := m.readLine(); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// screen membersihkan layar lalu menampilkan alamat panel dan menu.
func (m *Menu) screen() {
	host, _ := m.Hostname()
	addrs := m.Addrs()
	addr := m.t("noAddress")
	if len(addrs) > 0 {
		addr = strings.Join(addrs, ", ")
	}
	m.printf("\033[H\033[2J%s\n\n%s\n", m.t("title"), m.t("host", host, addr))
	// Alamat panel dan sidik jari sertifikat (ditulis sapod untuk layar
	// login; \4 = alamat IPv4, \n = nama host seperti di agetty).
	if data, err := os.ReadFile(m.Cfg.Agent.Console.IssueFile); err == nil {
		first := host
		if len(addrs) > 0 {
			first = addrs[0]
		}
		text := strings.NewReplacer(`\4`, first, `\n`, host).Replace(string(data))
		m.printf("%s", printable(text))
	}
	m.printf("\n%s\n\n%s\n%s", m.t("menu"), m.t("devLogin"), m.t("choose"))
}

// printable membuang karakter kendali selain baris baru (tidak ada escape
// terminal dari file).
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
}

func (m *Menu) fail(err error) bool {
	m.printf("\n%s\n", m.t("failed", err.Error()))
	return true
}

// --- Reset password admin ----------------------------------------------------

func (m *Menu) resetAdmin(ctx context.Context) bool {
	st, err := auth.OpenStore(m.Cfg.API.DataDir)
	if err != nil {
		return m.fail(err)
	}
	var admins []string
	for _, u := range st.Users() {
		if u.Role == auth.RoleAdmin {
			admins = append(admins, u.Username)
		}
	}
	if len(admins) == 0 {
		m.printf("\n%s\n", m.t("noAdmin"))
		return true
	}
	m.printf("\n%s\n\n%s\n", m.t("resetAdminHelp"), m.t("admins"))
	for i, a := range admins {
		m.printf("  %d. %s\n", i+1, a)
	}
	name := admins[0]
	if len(admins) > 1 {
		m.printf("%s", m.t("pickAdmin"))
		pick, err := m.readLine()
		if err != nil {
			return true
		}
		var n int
		if _, err := fmt.Sscan(pick, &n); err != nil || n < 1 || n > len(admins) {
			m.printf("%s\n", m.t("cancelled"))
			return true
		}
		name = admins[n-1]
	}
	m.printf("%s", m.t("typeUsername", name))
	if typed, err := m.readLine(); err != nil || typed != name {
		m.printf("%s\n", m.t("cancelled"))
		return true
	}
	password, err := tempPassword(m.Cfg.Recovery.TempPasswordLength)
	if err != nil {
		return m.fail(err)
	}
	a := m.Cfg.Auth.Argon2
	hash, err := auth.HashPassword(password, auth.Argon2Params{MemoryKiB: a.MemoryKiB, Iterations: a.Iterations, Parallelism: a.Parallelism})
	if err != nil {
		return m.fail(err)
	}
	m.printf("\n%s\n", m.t("working"))
	// Panel dihentikan dulu: ia menyimpan data akun di memori dan akan
	// menimpa perubahan dari luar. Semua sesi panel ikut berakhir.
	if err := m.systemctl(ctx, "stop", m.Cfg.Recovery.APIService); err != nil {
		return m.fail(err)
	}
	resetErr := m.applyAdminReset(name, hash)
	if err := m.systemctl(ctx, "start", m.Cfg.Recovery.APIService); err != nil && resetErr == nil {
		resetErr = err
	}
	if resetErr != nil {
		return m.fail(resetErr)
	}
	m.printf("\n%s\n\n    %s\n\n%s\n", m.t("resetAdminDone", name), password, m.t("resetAdminNext"))
	return true
}

// applyAdminReset mengubah data akun (panel sudah berhenti) dan mencatatnya
// di log audit. File data tetap milik user panel.
func (m *Menu) applyAdminReset(name, hash string) error {
	dir := m.Cfg.API.DataDir
	st, err := auth.OpenStore(dir)
	if err != nil {
		return err
	}
	resetErr := st.RecoveryReset(name, hash)
	m.audit("recovery.reset_admin", resetErr == nil, map[string]string{"user": name})
	if err := keepOwner(dir, "users.json", "audit.log", "audit.log.1"); err != nil && resetErr == nil {
		resetErr = err
	}
	return resetErr
}

// tempPassword membuat password sementara dari huruf dan angka yang tidak
// mudah tertukar, dikelompokkan per empat dengan tanda hubung.
func tempPassword(n int) (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(alphabet[k.Int64()])
	}
	return b.String(), nil
}

// keepOwner menyamakan pemilik file data dengan pemilik direktorinya (user
// panel), karena menu berjalan sebagai root.
func keepOwner(dir string, names ...string) error {
	uid, gid, err := owner(dir)
	if err != nil {
		return err
	}
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.Lchown(p, uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// audit mencatat aksi menu darurat (pelaku "console"). Kegagalan menulis
// tidak membatalkan pemulihan: tanpa menu ini NAS tidak bisa dipulihkan.
func (m *Menu) audit(action string, ok bool, detail map[string]string) {
	l, err := audit.Open(m.Cfg.API.DataDir, int64(m.Cfg.API.AuditMaxMB)<<20)
	if err != nil {
		return
	}
	defer l.Close()
	_ = l.Record(audit.Event{Time: m.Now().UTC(), Actor: "console", Action: action, IP: "console", OK: ok, Detail: detail})
	_ = keepOwner(m.Cfg.API.DataDir, "audit.log", "audit.log.1")
}

// --- Restart dan matikan -----------------------------------------------------

func (m *Menu) power(ctx context.Context, action, confirm string) bool {
	m.printf("\n%s", m.t(confirm))
	if typed, err := m.readLine(); err != nil || typed != "y" {
		m.printf("%s\n", m.t("cancelled"))
		return true
	}
	m.audit("recovery."+action, true, nil)
	if err := m.Run(ctx, m.Cfg.Recovery.Systemctl, action); err != nil {
		return m.fail(err)
	}
	return false
}

func (m *Menu) systemctl(ctx context.Context, action, unit string) error {
	return m.Run(ctx, m.Cfg.Recovery.Systemctl, action, "--", unit)
}
