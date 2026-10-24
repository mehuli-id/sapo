package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// ConsoleTools mengatur operasi console.* (dari agent.console di sapo.yaml).
type ConsoleTools struct {
	IssueFile string // ditulis sapod; /etc/issue.d/sapo.issue menautkan ke sini
	Agetty    string
	Timeout   time.Duration
}

var fingerprintPattern = regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`)

// ConsolePanelAddressOp mengembalikan handler console.panel_address: alamat
// panel dan sidik jari sertifikat untuk layar login konsol. Isinya dibuat dari
// templat tetap di sini; dari sapo-api hanya skema, port, dan sidik jari yang
// sudah divalidasi, sehingga sapo-api tidak bisa menulis teks bebas (atau
// escape terminal) ke layar yang dibaca root (ADR 0002, ADR 0004).
func ConsolePanelAddressOp(t ConsoleTools) Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.ConsolePanelParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		text, err := consoleText(p)
		if err != nil {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		if err := writeRootFile(t.IssueFile, []byte(text)); err != nil {
			return nil, err
		}
		// Layar login yang sedang tampil dicetak ulang dengan isi baru. Gagal
		// di sini (misalnya tidak ada agetty) tidak membatalkan operasi.
		if _, err := runTool(ctx, t.Timeout, t.Agetty, []string{"--reload"}); err != nil {
			return map[string]any{"ok": true, "reload": err.Error()}, nil
		}
		return map[string]any{"ok": true}, nil
	}
}

func consoleText(p proto.ConsolePanelParams) (string, error) {
	if p.Port < 1 || p.Port > 65535 {
		return "", fmt.Errorf("port tidak valid")
	}
	var b strings.Builder
	// agetty mengganti \4 dengan alamat IPv4 dan \n dengan nama host.
	switch p.Scheme {
	case "https":
		if !fingerprintPattern.MatchString(p.Fingerprint) {
			return "", fmt.Errorf("sidik jari tidak valid")
		}
		fmt.Fprintf(&b, "Panel Sapo: https://\\4:%d  atau  https://\\n.local:%d\n", p.Port, p.Port)
		// 95 karakter dipecah dua baris di titik dua ke-16.
		fmt.Fprintf(&b, "Sidik jari sertifikat (SHA-256), cocokkan dengan peringatan browser:\n  %s\n  %s\n\n",
			p.Fingerprint[:47], p.Fingerprint[48:])
	case "http":
		if p.Fingerprint != "" {
			return "", fmt.Errorf("sidik jari hanya untuk https")
		}
		fmt.Fprintf(&b, "Panel Sapo: http://\\4:%d  atau  http://\\n.local:%d\n\n", p.Port, p.Port)
	default:
		return "", fmt.Errorf("skema harus http atau https")
	}
	return b.String(), nil
}

// writeRootFile menulis file 0644 secara atomik di direktori milik sapod.
// rename mengganti entri direktori (termasuk symlink) tanpa mengikutinya.
func writeRootFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := checkSocketDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".console-*")
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}
