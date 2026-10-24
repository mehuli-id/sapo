// Package agent berisi sapod: satu-satunya komponen Sapo yang boleh mengubah
// sistem. Ia hanya mendengarkan unix socket lokal dan hanya menjalankan
// operasi bernama yang terdaftar di Registry (ADR 0002).
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
	"golang.org/x/sys/unix"
)

// Handler menjalankan satu operasi. params sudah dibatasi ukurannya; handler
// wajib mendekode ke struct bertipe dan memvalidasinya sendiri.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// FileResult adalah hasil operasi beserta satu file descriptor yang
// diteruskan ke klien lewat SCM_RIGHTS (misalnya file yang diunduh di File
// Station, dibuka atas nama pengguna). File ditutup setelah dikirim.
type FileResult struct {
	Result any
	File   *os.File
}

// Registry memetakan nama operasi ke handler-nya.
type Registry struct {
	mu  sync.RWMutex
	ops map[string]Handler
}

// NewRegistry membuat registry kosong.
func NewRegistry() *Registry { return &Registry{ops: map[string]Handler{}} }

// Register mendaftarkan operasi. Nama ganda adalah bug pemrograman.
func (r *Registry) Register(op string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.ops[op]; dup {
		panic("agent: operasi terdaftar dua kali: " + op)
	}
	r.ops[op] = h
}

func (r *Registry) lookup(op string) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.ops[op]
	return h, ok
}

// Ops mengembalikan daftar operasi yang dikenal.
func (r *Registry) Ops() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.ops))
	for k := range r.ops {
		out = append(out, k)
	}
	return out
}

// OpError adalah error dengan kode stabil untuk dikirim ke sapo-api.
type OpError = proto.Error

// Server melayani koneksi di unix socket.
type Server struct {
	Registry    *Registry
	Log         *slog.Logger
	OpTimeout   time.Duration
	IdleTimeout time.Duration
	// WriteTimeout membatasi waktu menulis jawaban; klien yang tidak
	// membaca tidak bisa menahan goroutine selamanya.
	WriteTimeout time.Duration
	// MaxConns membatasi koneksi bersamaan (0 = default).
	MaxConns int
}

const defaultMaxConns = 32

// maxEchoedOp membatasi nama operasi yang dikembalikan di pesan error.
const maxEchoedOp = 64

// Listen membuka unix socket di path dengan izin 0660 dan grup group.
//
// Direktori socket harus milik user yang menjalankan sapod dan tidak bisa
// ditulis grup/lainnya; bila tidak, sapod menolak berjalan (mencegah trik
// symlink yang membuat root mengubah izin file lain). Bila sapod berjalan
// sebagai root, grup wajib ada.
func Listen(path, group string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("membuat direktori socket: %w", err)
	}
	if err := checkSocketDir(dir); err != nil {
		return nil, err
	}
	gid := -1
	if group != "" {
		g, err := user.LookupGroup(group)
		switch {
		case err == nil:
			gid, _ = strconv.Atoi(g.Gid)
		case os.Geteuid() == 0:
			return nil, fmt.Errorf("grup socket %q tidak ada; sapo-api tidak akan bisa terhubung", group)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("menghapus socket lama: %w", err)
	}
	// Socket langsung dibuat dengan izin 0660 (tanpa jeda izin longgar).
	old := syscall.Umask(0o117)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if gid >= 0 {
		if err := os.Lchown(path, -1, gid); err != nil {
			ln.Close()
			return nil, fmt.Errorf("mengatur grup socket: %w", err)
		}
	}
	return ln, nil
}

func checkSocketDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("direktori socket %s bukan direktori (symlink tidak diizinkan)", dir)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("direktori socket %s bisa ditulis grup/lainnya (izin %v)", dir, st.Mode().Perm())
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Geteuid() {
		return fmt.Errorf("direktori socket %s bukan milik user sapod", dir)
	}
	return nil
}

// Serve menerima koneksi sampai ctx selesai. Error sementara (misalnya
// kehabisan file descriptor) tidak menghentikan sapod.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	max := s.MaxConns
	if max <= 0 {
		max = defaultMaxConns
	}
	slots := make(chan struct{}, max)
	backoff := 5 * time.Millisecond
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			if s.Log != nil {
				s.Log.Warn("accept gagal, mencoba lagi", "err", err, "tunggu", backoff)
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			backoff = min(backoff*2, time.Second)
			continue
		}
		backoff = 5 * time.Millisecond
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				s.handleConn(ctx, conn)
			}()
		default:
			// Terlalu banyak koneksi: tolak yang baru, jangan menumpuk.
			conn.Close()
		}
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), proto.MaxMessageBytes)
	enc := json.NewEncoder(conn)
	writeTimeout := s.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = 30 * time.Second
	}
	for {
		if s.IdleTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.IdleTimeout))
		}
		if !sc.Scan() {
			return
		}
		resp, file := s.dispatch(ctx, sc.Bytes())
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if file != nil {
			err := sendWithFile(conn, resp, file)
			file.Close()
			if err != nil {
				return
			}
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

// sendWithFile mengirim jawaban dan file descriptor dalam satu pesan.
func sendWithFile(conn net.Conn, resp proto.Response, f *os.File) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("file descriptor hanya bisa dikirim lewat unix socket")
	}
	line, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, _, err = uc.WriteMsgUnix(append(line, '\n'), unix.UnixRights(int(f.Fd())), nil)
	return err
}

func (s *Server) dispatch(ctx context.Context, line []byte) (proto.Response, *os.File) {
	var req proto.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return fail(0, proto.ErrBadRequest, "permintaan bukan JSON yang valid"), nil
	}
	h, ok := s.Registry.lookup(req.Op)
	if !ok {
		op := req.Op
		if len(op) > maxEchoedOp {
			op = op[:maxEchoedOp] + "…"
		}
		return fail(req.ID, proto.ErrUnknownOp, "operasi tidak dikenal: "+op), nil
	}
	opCtx := ctx
	if s.OpTimeout > 0 {
		var cancel context.CancelFunc
		opCtx, cancel = context.WithTimeout(ctx, s.OpTimeout)
		defer cancel()
	}
	start := time.Now()
	result, err := h(opCtx, req.Params)
	if err != nil {
		var oe *OpError
		if errors.As(err, &oe) {
			if s.Log != nil {
				s.Log.Info("operasi", "op", req.Op, "durasi", time.Since(start), "ok", false, "kode", oe.Code)
			}
			return fail(req.ID, oe.Code, oe.Message), nil
		}
		// Detail error internal hanya ke log sapod, tidak ke klien.
		if s.Log != nil {
			s.Log.Error("operasi gagal", "op", req.Op, "durasi", time.Since(start), "err", err)
		}
		return fail(req.ID, proto.ErrInternal, "kesalahan internal"), nil
	}
	var file *os.File
	if fr, ok := result.(FileResult); ok {
		result, file = fr.Result, fr.File
	}
	if s.Log != nil {
		s.Log.Info("operasi", "op", req.Op, "durasi", time.Since(start), "ok", true)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		if file != nil {
			file.Close()
		}
		return fail(req.ID, proto.ErrInternal, "gagal menyusun hasil"), nil
	}
	return proto.Response{ID: req.ID, OK: true, Result: raw}, file
}

func fail(id uint64, code, msg string) proto.Response {
	return proto.Response{ID: id, OK: false, Error: &proto.Error{Code: code, Message: msg}}
}
