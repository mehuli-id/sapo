package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

// maxToolOutput membatasi keluaran program yang dibaca sapod.
const maxToolOutput = 4 << 20

// toolEnv adalah lingkungan program anak: tetap dan minimal, tidak mewarisi
// lingkungan sapod. LC_ALL=C agar keluaran tidak berubah menurut bahasa.
var toolEnv = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}

// toolResult adalah hasil satu pemanggilan program.
type toolResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// errToolMissing: program tidak ada atau tidak bisa dijalankan.
var errToolMissing = errors.New("program tidak tersedia")

// runTool menjalankan program prog (path absolut) tanpa shell. flags adalah
// opsi tetap yang ditulis di kode; operands (misalnya path perangkat) selalu
// diletakkan setelah "--" sehingga tidak pernah dibaca sebagai opsi.
// Kode keluar bukan nol tidak dianggap error: banyak alat (misalnya smartctl)
// memakainya untuk melaporkan status, jadi pemanggil yang menafsirkannya.
func runTool(ctx context.Context, timeout time.Duration, prog string, flags []string, operands ...string) (toolResult, error) {
	if !filepath.IsAbs(prog) {
		return toolResult{}, fmt.Errorf("%w: %q bukan path absolut", errToolMissing, prog)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := make([]string, 0, len(flags)+1+len(operands))
	args = append(args, flags...)
	args = append(args, "--")
	args = append(args, operands...)

	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.Env = toolEnv
	cmd.Dir = "/"
	cmd.WaitDelay = time.Second
	stdout := &limitedBuffer{max: maxToolOutput}
	stderr := &limitedBuffer{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Run()
	res := toolResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return res, fmt.Errorf("%s melewati batas waktu %s", filepath.Base(prog), timeout)
	case stdout.overflow:
		return res, fmt.Errorf("keluaran %s melebihi %d byte", filepath.Base(prog), maxToolOutput)
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	case err != nil:
		return res, fmt.Errorf("%w: %s: %v", errToolMissing, filepath.Base(prog), err)
	}
	return res, nil
}

// limitedBuffer menyimpan paling banyak max byte; sisanya dibuang dan
// overflow ditandai, sehingga program tidak bisa menghabiskan memori sapod.
// bytes.Buffer sengaja tidak disematkan: ReadFrom miliknya akan dipakai
// io.Copy (os/exec) dan melewati batas di Write.
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); len(p) > room {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }
