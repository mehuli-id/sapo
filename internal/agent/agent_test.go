package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

func startServer(t *testing.T, reg *Registry) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "sapod.sock")
	ln, err := Listen(sock, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := &Server{Registry: reg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), OpTimeout: time.Second}
	go srv.Serve(ctx, ln)

	st, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o660 {
		t.Fatalf("izin socket = %v, harus 0660", st.Mode().Perm())
	}
	return &Client{Socket: sock, Timeout: 2 * time.Second}
}

func TestCallRoundTrip(t *testing.T) {
	reg := NewRegistry()
	reg.Register("test.echo", func(_ context.Context, p json.RawMessage) (any, error) {
		var in struct{ Text string }
		if err := json.Unmarshal(p, &in); err != nil {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		return map[string]string{"text": strings.ToUpper(in.Text)}, nil
	})
	c := startServer(t, reg)

	var out struct{ Text string }
	if err := c.Call(context.Background(), "test.echo", map[string]string{"Text": "sapo"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Text != "SAPO" {
		t.Fatalf("hasil = %q", out.Text)
	}
}

func TestUnknownOpIsRejected(t *testing.T) {
	c := startServer(t, NewRegistry())
	err := c.Call(context.Background(), "shell.exec", map[string]string{"cmd": "rm -rf /"}, nil)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.ErrUnknownOp {
		t.Fatalf("operasi tak dikenal harus ditolak dengan unknown_op, dapat %v", err)
	}
}

func TestOpErrorCodeIsPreserved(t *testing.T) {
	reg := NewRegistry()
	reg.Register("test.fail", func(context.Context, json.RawMessage) (any, error) {
		return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama pool kosong"}
	})
	c := startServer(t, reg)
	err := c.Call(context.Background(), "test.fail", nil, nil)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.ErrInvalidParams {
		t.Fatalf("kode error hilang: %v", err)
	}
}

func TestReadSystemInfoFromFixtures(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fsys := ProcFS{
		OSRelease: write("os-release", "NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\n"),
		Hostname:  write("hostname", "sapo-lab\n"),
		Kernel:    write("osrelease", "6.12.0-1-amd64\n"),
		Uptime:    write("uptime", "3600.50 7000.00\n"),
		CPUInfo:   write("cpuinfo", "processor\t: 0\nmodel name\t: Intel N100\n\nprocessor\t: 1\nmodel name\t: Intel N100\n"),
		MemInfo:   write("meminfo", "MemTotal:       16000000 kB\nMemFree:  100 kB\nMemAvailable:   12000000 kB\n"),
		LoadAvg:   write("loadavg", "0.10 0.20 0.30 1/200 12345\n"),
	}
	info := ReadSystemInfo(fsys, "0.0.0-dev")
	switch {
	case info.Hostname != "sapo-lab":
		t.Fatalf("hostname %q", info.Hostname)
	case info.OS != "Debian GNU/Linux 13 (trixie)":
		t.Fatalf("os %q", info.OS)
	case info.CPU.Model != "Intel N100" || info.CPU.Cores != 2:
		t.Fatalf("cpu %+v", info.CPU)
	case info.Memory.TotalBytes != 16000000*1024 || info.Memory.AvailableBytes != 12000000*1024:
		t.Fatalf("memory %+v", info.Memory)
	case info.UptimeSeconds != 3600.5:
		t.Fatalf("uptime %v", info.UptimeSeconds)
	case len(info.LoadAverage) != 3 || info.LoadAverage[2] != 0.30:
		t.Fatalf("load %v", info.LoadAverage)
	}
}

func TestShippedFeatureManifestsAreValid(t *testing.T) {
	features, err := LoadFeatures("../../features")
	if err != nil {
		t.Fatal(err)
	}
	if len(features) == 0 {
		t.Fatal("tidak ada manifest fitur")
	}
}

func TestFeatureManifestRejectsShellInjection(t *testing.T) {
	dir := t.TempDir()
	bad := "id: jahat\nname: { id: Jahat, en: Evil }\ngroup: x\nactivation: optional\npackages: [\"samba; rm -rf /\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "jahat.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFeatures(dir); err == nil {
		t.Fatal("nama paket berbahaya harus ditolak")
	}
}
