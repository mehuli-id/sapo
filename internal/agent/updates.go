package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Pembaruan sistem (ADR 0011). Semua argumen apt-get tetap (ditulis di
// kode), tidak ada masukan dari panel selain perintah "periksa" dan
// "pasang".

// UpdatesTools mengatur operasi updates.* (agent.updates di sapo.yaml).
type UpdatesTools struct {
	AptGet, SystemdRun, Systemctl string
	// Self: program sapod (dijalankan systemd-run dengan argumen apt-run).
	Self string
	// Unit: unit systemd sementara tempat pemasangan berjalan, terpisah dari
	// sapod, agar tidak ikut dihentikan bila paket sapo sendiri diperbarui.
	Unit string
	// LogFile dan ResultFile ditulis oleh apt-run.
	LogFile, ResultFile string
	CheckTimeout        time.Duration
}

// Updates melayani updates.check, updates.install, dan updates.status.
type Updates struct {
	t  UpdatesTools
	mu sync.Mutex
	// run menjalankan program dengan argumen tetap (diganti di test).
	run func(ctx context.Context, prog string, args ...string) (toolResult, error)
}

// NewUpdates menyiapkan pembaruan sistem.
func NewUpdates(t UpdatesTools) *Updates {
	return &Updates{t: t, run: runFixed}
}

// aptEnv: apt dan dpkg tanpa pertanyaan interaktif.
var aptEnv = append([]string{"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"}, toolEnv...)

// runFixed menjalankan program dengan argumen tetap dari kode (bukan dari
// masukan), tanpa shell.
func runFixed(ctx context.Context, prog string, args ...string) (toolResult, error) {
	if !filepath.IsAbs(prog) {
		return toolResult{}, fmt.Errorf("%w: %q bukan path absolut", errToolMissing, prog)
	}
	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.Env = aptEnv
	cmd.Dir = "/"
	stdout := &limitedBuffer{max: maxToolOutput}
	stderr := &limitedBuffer{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	res := toolResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return res, fmt.Errorf("%s melewati batas waktu", filepath.Base(prog))
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	case err != nil:
		return res, fmt.Errorf("%w: %s: %v", errToolMissing, filepath.Base(prog), err)
	}
	return res, nil
}

// running: unit pemasangan sedang aktif.
func (u *Updates) running(ctx context.Context) bool {
	res, err := u.run(ctx, u.t.Systemctl, "is-active", "--quiet", u.t.Unit)
	return err == nil && res.ExitCode == 0
}

// instLine: baris simulasi apt-get "Inst nama [lama] (baru suite [arch])".
var instLine = regexp.MustCompile(`^Inst (\S+) (?:\[([^\]]+)\] )?\((\S+) `)

// parseSimulation membaca daftar paket dari apt-get -s.
func parseSimulation(out []byte) []proto.UpdatePackage {
	pkgs := []proto.UpdatePackage{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if m := instLine.FindStringSubmatch(sc.Text()); m != nil {
			pkgs = append(pkgs, proto.UpdatePackage{Name: m[1], From: m[2], To: m[3]})
		}
	}
	return pkgs
}

// upgradeArgs: memperbarui paket terpasang (boleh memasang paket baru yang
// dibutuhkan, tidak pernah menghapus paket), konfigurasi yang diubah admin
// dipertahankan.
var upgradeArgs = []string{"-q", "-y", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "upgrade", "--with-new-pkgs"}

// CheckOp mengembalikan handler updates.check.
func (u *Updates) CheckOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		if !u.mu.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "pemeriksaan pembaruan sedang berjalan"}
		}
		defer u.mu.Unlock()
		ctx, cancel := context.WithTimeout(ctx, u.t.CheckTimeout)
		defer cancel()
		if u.running(ctx) {
			return nil, &OpError{Code: proto.ErrBusy, Message: "pembaruan sedang dipasang"}
		}
		res, err := u.run(ctx, u.t.AptGet, "-q", "update")
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, &OpError{Code: "updates_failed", Message: clipTo(lastLines(res.Stderr, 3), maxErrorText)}
		}
		sim := append([]string{"-s", "-o", "Debug::NoLocking=1"}, upgradeArgs...)
		res, err = u.run(ctx, u.t.AptGet, sim...)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, &OpError{Code: "updates_failed", Message: clipTo(lastLines(res.Stderr, 3), maxErrorText)}
		}
		return proto.UpdatesCheckResult{CheckedAt: time.Now().UTC(), Packages: parseSimulation(res.Stdout)}, nil
	}
}

// InstallOp mengembalikan handler updates.install: apt-run dijalankan di
// unit systemd sementara, sapod tidak menunggu.
func (u *Updates) InstallOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		if !u.mu.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "pemeriksaan pembaruan sedang berjalan"}
		}
		defer u.mu.Unlock()
		if u.running(ctx) {
			return nil, &OpError{Code: proto.ErrBusy, Message: "pembaruan sedang dipasang"}
		}
		_ = os.Remove(u.t.ResultFile)
		res, err := u.run(ctx, u.t.SystemdRun,
			"--unit="+u.t.Unit, "--description=Pembaruan sistem Sapo", "--collect", "--quiet",
			"--property=Type=exec", "--",
			u.t.Self, "apt-run", u.t.AptGet, u.t.LogFile, u.t.ResultFile)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, &OpError{Code: "updates_failed", Message: clipTo(firstLine(res.Stderr), maxErrorText)}
		}
		return map[string]bool{"ok": true}, nil
	}
}

// StatusOp mengembalikan handler updates.status.
func (u *Updates) StatusOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		out := proto.UpdatesStatus{Running: u.running(ctx), Log: []string{}}
		if data, err := os.ReadFile(u.t.ResultFile); err == nil {
			var r proto.UpdateRun
			if json.Unmarshal(data, &r) == nil {
				out.Last = &r
			}
		}
		if f, err := os.Open(u.t.LogFile); err == nil {
			defer f.Close()
			if st, err := f.Stat(); err == nil && st.Size() > 32<<10 {
				_, _ = f.Seek(st.Size()-32<<10, io.SeekStart)
			}
			b, _ := io.ReadAll(io.LimitReader(f, 32<<10))
			lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			if len(lines) > 40 {
				lines = lines[len(lines)-40:]
			}
			for _, l := range lines {
				out.Log = append(out.Log, printableLine(l))
			}
		}
		return out, nil
	}
}

// printableLine membuang karakter kendali (keluaran apt untuk terminal).
func printableLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// AptRunMain adalah "sapod apt-run <apt-get> <log> <hasil>": dijalankan
// systemd-run sebagai root di unit tersendiri. Menjalankan apt-get update
// lalu upgrade, mencatat keluaran ke log dan hasil ke file hasil.
func AptRunMain(args []string) int {
	if len(args) != 3 || !filepath.IsAbs(args[0]) || !filepath.IsAbs(args[1]) || !filepath.IsAbs(args[2]) {
		fmt.Fprintln(os.Stderr, "pemakaian: sapod apt-run <apt-get> <log> <hasil>")
		return 2
	}
	aptGet, logPath, resultPath := args[0], args[1], args[2]
	run := proto.UpdateRun{StartedAt: time.Now().UTC()}
	writeResult := func() {
		data, _ := json.Marshal(run)
		_ = writePrivateFile(resultPath, data)
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		run.ExitCode = -1
		run.FinishedAt = time.Now().UTC()
		writeResult()
		return 1
	}
	defer log.Close()
	writeResult()
	for _, a := range [][]string{{"-q", "update"}, upgradeArgs} {
		fmt.Fprintf(log, "$ apt-get %s\n", strings.Join(a, " "))
		cmd := exec.Command(aptGet, a...)
		cmd.Env = aptEnv
		cmd.Dir = "/"
		cmd.Stdout, cmd.Stderr = log, log
		err := cmd.Run()
		var exitErr *exec.ExitError
		switch {
		case errors.As(err, &exitErr):
			run.ExitCode = exitErr.ExitCode()
		case err != nil:
			run.ExitCode = -1
			fmt.Fprintln(log, err)
		}
		if run.ExitCode != 0 {
			break
		}
	}
	run.OK = run.ExitCode == 0
	run.FinishedAt = time.Now().UTC()
	fmt.Fprintf(log, "selesai: kode %d\n", run.ExitCode)
	writeResult()
	if !run.OK {
		return 1
	}
	return 0
}
