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

const testFingerprint = "73:3E:5E:53:5C:9F:E7:44:63:5D:8D:35:8D:9C:C9:52:C9:B1:27:06:76:9A:9A:E4:FD:0C:33:CA:71:D7:D2:1F"

func consoleTools(t *testing.T) (ConsoleTools, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	agetty := fakeTool(t, t.TempDir(), "agetty", "", 0)
	return ConsoleTools{IssueFile: filepath.Join(dir, "console.issue"), Agetty: agetty, Timeout: 5 * time.Second}, agetty
}

func TestConsolePanelAddressWritesTemplate(t *testing.T) {
	tools, agetty := consoleTools(t)
	op := ConsolePanelAddressOp(tools)
	if _, err := op(context.Background(), json.RawMessage(`{"scheme":"https","port":5001,"fingerprint":"`+testFingerprint+`"}`)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(tools.IssueFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	// Sidik jari utuh dalam dua baris, alamat memakai escape agetty.
	if !strings.Contains(text, "https://\\4:5001") || !strings.Contains(text, testFingerprint[:47]) || !strings.Contains(text, testFingerprint[48:]) {
		t.Fatalf("isi layar konsol:\n%s", text)
	}
	if st, _ := os.Stat(tools.IssueFile); st.Mode().Perm() != 0o644 {
		t.Fatalf("izin file %v, harus 0644", st.Mode().Perm())
	}
	if args := toolArgs(t, agetty); len(args) < 1 || args[0] != "--reload" {
		t.Fatalf("agetty harus diminta --reload, argumen %q", args)
	}
}

func TestConsolePanelAddressRejectsUntrustedText(t *testing.T) {
	tools, _ := consoleTools(t)
	op := ConsolePanelAddressOp(tools)
	for _, p := range []string{
		`{"scheme":"https","port":5001,"fingerprint":"\u001b[2J palsu"}`,
		`{"scheme":"https","port":5001,"fingerprint":"` + strings.ToLower(testFingerprint) + `"}`,
		`{"scheme":"https","port":5001}`,
		`{"scheme":"http","port":5000,"fingerprint":"` + testFingerprint + `"}`,
		`{"scheme":"ftp","port":21}`,
		`{"scheme":"https","port":0,"fingerprint":"` + testFingerprint + `"}`,
		`{"scheme":"https","port":70000,"fingerprint":"` + testFingerprint + `"}`,
		`{"scheme":"http","port":5000,"text":"bebas"}`,
	} {
		_, err := op(context.Background(), json.RawMessage(p))
		var oe *OpError
		if !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
			t.Errorf("%s: harus ditolak, dapat %v", p, err)
		}
	}
	if _, err := os.Stat(tools.IssueFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("permintaan yang ditolak tidak boleh menulis file")
	}
}

func TestConsolePanelAddressReplacesSymlinkWithoutFollowing(t *testing.T) {
	tools, _ := consoleTools(t)
	victim := filepath.Join(t.TempDir(), "rahasia")
	if err := os.WriteFile(victim, []byte("jangan diubah"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, tools.IssueFile); err != nil {
		t.Fatal(err)
	}
	if _, err := ConsolePanelAddressOp(tools)(context.Background(), json.RawMessage(`{"scheme":"http","port":5000}`)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "jangan diubah" {
		t.Fatal("symlink tidak boleh diikuti saat menulis")
	}
	if st, _ := os.Lstat(tools.IssueFile); st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("symlink harus diganti file biasa")
	}
}

func TestConsolePanelAddressRefusesWritableDir(t *testing.T) {
	tools, _ := consoleTools(t)
	if err := os.Chmod(filepath.Dir(tools.IssueFile), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := ConsolePanelAddressOp(tools)(context.Background(), json.RawMessage(`{"scheme":"http","port":5000}`)); err == nil {
		t.Fatal("direktori yang bisa ditulis siapa saja harus ditolak")
	}
}
