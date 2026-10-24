package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/mehuli-id/sapo/internal/audit"
	"github.com/mehuli-id/sapo/internal/auth"
)

// Data adalah akses menu ke data panel (akun dan log audit) di
// <api.data_dir>. Direktori itu milik user panel (sapo), yang bisa saja
// disusupi dan menanam symlink atau hardlink di sana; jadi menu yang
// berjalan sebagai root tidak pernah membuka file di dalamnya sendiri.
type Data interface {
	// Admins: nama administrator dan bahasa setup awal.
	Admins() (admins []string, language string, err error)
	ResetAdmin(name, hash string) error
	Audit(e audit.Event)
}

// localData membaca dan menulis data panel langsung, dengan hak proses ini
// (dipakai proses pembantu yang sudah berjalan sebagai pemilik direktori,
// dan oleh test).
type localData struct {
	dir      string
	auditMax int64
}

func (d localData) Admins() ([]string, string, error) {
	st, err := auth.OpenStore(d.dir)
	if err != nil {
		return nil, "", err
	}
	var admins []string
	for _, u := range st.Users() {
		if u.Role == auth.RoleAdmin {
			admins = append(admins, u.Username)
		}
	}
	return admins, st.Language(), nil
}

func (d localData) ResetAdmin(name, hash string) error {
	st, err := auth.OpenStore(d.dir)
	if err != nil {
		return err
	}
	return st.RecoveryReset(name, hash)
}

func (d localData) Audit(e audit.Event) {
	l, err := audit.Open(d.dir, d.auditMax)
	if err != nil {
		return
	}
	defer l.Close()
	_ = l.Record(e)
}

// helperData menjalankan setiap akses lewat proses pembantu (program ini
// sendiri, "sapo-console helper …") yang berjalan sebagai pemilik direktori
// data, tanpa grup tambahan. Symlink atau hardlink yang ditanam di sana
// hanya bisa menyentuh file yang memang bisa ditulis user itu.
type helperData struct {
	exe      string
	dir      string
	auditMax int64
	uid, gid uint32
}

// NewHelperData menyiapkan akses data panel lewat proses pembantu.
func NewHelperData(dir string, auditMax int64) (Data, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok {
		return nil, fmt.Errorf("%s bukan direktori", dir)
	}
	return helperData{exe: exe, dir: dir, auditMax: auditMax, uid: st.Uid, gid: st.Gid}, nil
}

func (h helperData) call(cmd string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	c := exec.CommandContext(context.Background(), h.exe, "helper", "-data", h.dir, "-audit-max", strconv.FormatInt(h.auditMax, 10), cmd)
	c.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	c.Dir = "/"
	c.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: h.uid, Gid: h.gid, Groups: []uint32{}}}
	if err := c.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(printable(firstLine(msg)))
		}
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(stdout.Bytes(), out)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

type adminsReply struct {
	Admins   []string `json:"admins"`
	Language string   `json:"language"`
}

type resetRequest struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

func (h helperData) Admins() ([]string, string, error) {
	var r adminsReply
	err := h.call("admins", struct{}{}, &r)
	return r.Admins, r.Language, err
}

func (h helperData) ResetAdmin(name, hash string) error {
	return h.call("reset", resetRequest{Name: name, Hash: hash}, nil)
}

func (h helperData) Audit(e audit.Event) { _ = h.call("audit", e, nil) }

// HelperMain adalah "sapo-console helper": satu perintah (admins, reset,
// audit) dengan permintaan JSON di stdin. Mengembalikan kode keluar.
func HelperMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("helper", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("data", "", "direktori data panel")
	auditMax := fs.Int64("audit-max", 0, "batas ukuran log audit (byte)")
	if err := fs.Parse(args); err != nil || *dir == "" || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "pemakaian: sapo-console helper -data DIR [-audit-max N] admins|reset|audit")
		return 2
	}
	d := localData{dir: *dir, auditMax: *auditMax}
	dec := json.NewDecoder(io.LimitReader(stdin, 1<<16))
	var err error
	switch fs.Arg(0) {
	case "admins":
		var r adminsReply
		r.Admins, r.Language, err = d.Admins()
		if err == nil {
			err = json.NewEncoder(stdout).Encode(r)
		}
	case "reset":
		var req resetRequest
		if err = dec.Decode(&req); err == nil {
			err = d.ResetAdmin(req.Name, req.Hash)
		}
	case "audit":
		var e audit.Event
		if err = dec.Decode(&e); err == nil {
			d.Audit(e)
		}
	default:
		err = fmt.Errorf("perintah tidak dikenal: %s", fs.Arg(0))
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
