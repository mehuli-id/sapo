package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestParseSimulation(t *testing.T) {
	out := []byte(`Reading package lists...
Inst libssl3t64 [3.5.1-1] (3.5.4-1~deb13u1 Debian-Security:13/stable-security [amd64])
Inst sapo [0.4.0] (0.5.0 sapo:dev [arm64])
Inst linux-image-6.12.48 (6.12.48-1 Debian:13.1/stable [amd64])
Conf libssl3t64 (3.5.4-1~deb13u1 Debian-Security:13/stable-security [amd64])
`)
	got := parseSimulation(out)
	want := []proto.UpdatePackage{{Name: "libssl3t64", From: "3.5.1-1", To: "3.5.4-1~deb13u1"}, {Name: "sapo", From: "0.4.0", To: "0.5.0"}, {Name: "linux-image-6.12.48", To: "6.12.48-1"}}
	if len(got) != len(want) {
		t.Fatalf("paket = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("paket %d = %+v, mau %+v", i, got[i], want[i])
		}
	}
}

func TestUpdatesOps(t *testing.T) {
	dir := t.TempDir()
	active := false
	var calls []string
	u := NewUpdates(UpdatesTools{AptGet: "/usr/bin/apt-get", SystemdRun: "/usr/bin/systemd-run", Systemctl: "/usr/bin/systemctl",
		Self: "/usr/lib/sapo/sapod", Unit: "sapo-updates.service", LogFile: filepath.Join(dir, "updates.log"),
		ResultFile: filepath.Join(dir, "result.json"), CheckTimeout: time.Minute})
	u.run = func(_ context.Context, prog string, args ...string) (toolResult, error) {
		calls = append(calls, filepath.Base(prog)+" "+strings.Join(args, " "))
		switch {
		case filepath.Base(prog) == "systemctl":
			if active {
				return toolResult{}, nil
			}
			return toolResult{ExitCode: 3}, nil
		case filepath.Base(prog) == "apt-get" && args[0] == "-s":
			return toolResult{Stdout: []byte("Inst curl [8.14.1-1] (8.14.1-2 Debian:13/stable [amd64])\n")}, nil
		}
		return toolResult{}, nil
	}
	res, err := u.CheckOp()(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.(proto.UpdatesCheckResult); len(r.Packages) != 1 || r.Packages[0].Name != "curl" {
		t.Fatalf("periksa = %+v", r)
	}
	// Simulasi tanpa menghapus paket dan tanpa kunci dpkg.
	if !strings.Contains(strings.Join(calls, "\n"), "apt-get -s -o Debug::NoLocking=1 -q -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold upgrade --with-new-pkgs") {
		t.Fatalf("panggilan:\n%s", strings.Join(calls, "\n"))
	}
	if _, err := u.InstallOp()(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	last := calls[len(calls)-1]
	if !strings.HasPrefix(last, "systemd-run --unit=sapo-updates.service") || !strings.HasSuffix(last, "-- /usr/lib/sapo/sapod apt-run /usr/bin/apt-get "+filepath.Join(dir, "updates.log")+" "+filepath.Join(dir, "result.json")) {
		t.Fatalf("systemd-run = %s", last)
	}
	// Sedang berjalan: periksa dan pasang ditolak.
	active = true
	var oe *OpError
	if _, err := u.InstallOp()(context.Background(), nil); !errors.As(err, &oe) || oe.Code != proto.ErrBusy {
		t.Fatalf("pasang kedua: %v", err)
	}
	if _, err := u.CheckOp()(context.Background(), nil); !errors.As(err, &oe) || oe.Code != proto.ErrBusy {
		t.Fatalf("periksa saat memasang: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "updates.log"), []byte("$ apt-get upgrade\nSetting up curl \x1b[1m(8.14.1-2)\nselesai: kode 0\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"started_at":"2026-10-25T01:00:00Z","ok":true,"exit_code":0}`), 0o600)
	res, _ = u.StatusOp()(context.Background(), nil)
	st := res.(proto.UpdatesStatus)
	if !st.Running || st.Last == nil || !st.Last.OK || len(st.Log) != 3 || strings.ContainsRune(st.Log[1], 0x1b) {
		t.Fatalf("status = %+v", st)
	}
}

func TestAptRunMain(t *testing.T) {
	dir := t.TempDir()
	apt := filepath.Join(dir, "apt-get")
	_ = os.WriteFile(apt, []byte("#!/bin/sh\necho \"apt $*\"\n[ \"$DEBIAN_FRONTEND\" = noninteractive ] || exit 9\ncase \"$*\" in *upgrade*) exit 100 ;; esac\n"), 0o755)
	log, result := filepath.Join(dir, "log"), filepath.Join(dir, "result.json")
	if code := AptRunMain([]string{apt, log, result}); code != 1 {
		t.Fatalf("kode = %d", code)
	}
	var r proto.UpdateRun
	b, _ := os.ReadFile(result)
	_ = json.Unmarshal(b, &r)
	if r.OK || r.ExitCode != 100 || r.FinishedAt.IsZero() {
		t.Fatalf("hasil = %+v", r)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "apt -q update") || !strings.Contains(string(b), "selesai: kode 100") {
		t.Fatalf("log:\n%s", b)
	}
	if code := AptRunMain([]string{"apt-get", log, result}); code != 2 {
		t.Fatal("path relatif harus ditolak")
	}
}
