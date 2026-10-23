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
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Handler menjalankan satu operasi. params sudah dibatasi ukurannya; handler
// wajib mendekode ke struct bertipe dan memvalidasinya sendiri.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

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
}

// Listen membuka unix socket di path dengan izin 0660 dan grup group
// (bila grup ada). Socket lama di path yang sama dihapus dulu.
func Listen(path, group string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("membuat direktori socket: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("menghapus socket lama: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, fmt.Errorf("mengatur izin socket: %w", err)
	}
	if group != "" {
		if g, err := user.LookupGroup(group); err == nil {
			gid, _ := strconv.Atoi(g.Gid)
			if err := os.Chown(path, -1, gid); err != nil {
				ln.Close()
				return nil, fmt.Errorf("mengatur grup socket: %w", err)
			}
		}
	}
	return ln, nil
}

// Serve menerima koneksi sampai ctx selesai.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), proto.MaxMessageBytes)
	enc := json.NewEncoder(conn)
	for {
		if s.IdleTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.IdleTimeout))
		}
		if !sc.Scan() {
			return
		}
		resp := s.dispatch(ctx, sc.Bytes())
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) dispatch(ctx context.Context, line []byte) proto.Response {
	var req proto.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return fail(0, proto.ErrBadRequest, "permintaan bukan JSON yang valid")
	}
	h, ok := s.Registry.lookup(req.Op)
	if !ok {
		return fail(req.ID, proto.ErrUnknownOp, "operasi tidak dikenal: "+req.Op)
	}
	opCtx := ctx
	if s.OpTimeout > 0 {
		var cancel context.CancelFunc
		opCtx, cancel = context.WithTimeout(ctx, s.OpTimeout)
		defer cancel()
	}
	start := time.Now()
	result, err := h(opCtx, req.Params)
	if s.Log != nil {
		s.Log.Info("operasi", "op", req.Op, "durasi", time.Since(start), "ok", err == nil)
	}
	if err != nil {
		var oe *OpError
		if errors.As(err, &oe) {
			return fail(req.ID, oe.Code, oe.Message)
		}
		return fail(req.ID, proto.ErrInternal, err.Error())
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return fail(req.ID, proto.ErrInternal, "gagal menyusun hasil")
	}
	return proto.Response{ID: req.ID, OK: true, Result: raw}
}

func fail(id uint64, code, msg string) proto.Response {
	return proto.Response{ID: id, OK: false, Error: &proto.Error{Code: code, Message: msg}}
}
