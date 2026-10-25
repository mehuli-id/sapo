// Package mail mengirim email notifikasi lewat SMTP (ADR 0018).
//
// Keamanan koneksi dipilih admin: "starttls" (wajib; server tanpa STARTTLS
// ditolak, tidak turun ke teks biasa), "tls" (TLS langsung, biasanya port
// 465), atau "none" (tanpa enkripsi, misalnya relay di jaringan lokal).
// Login (username/password) hanya dikirim lewat koneksi terenkripsi.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Server adalah pengaturan server SMTP.
type Server struct {
	Host     string
	Port     int
	Security string // starttls | tls | none
	Username string
	Password string
	// RootCAs: nil = sertifikat sistem (diganti di test).
	tlsConfig *tls.Config
}

// Message adalah satu email teks biasa.
type Message struct {
	From    string
	To      []string
	Subject string
	Body    string
}

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$|^\[?[0-9A-Fa-f:.]+\]?$`)

// ValidServer memeriksa host, port, dan mode keamanan.
func ValidServer(s Server) error {
	switch {
	case !hostPattern.MatchString(s.Host):
		return errors.New("nama server tidak valid")
	case s.Port < 1 || s.Port > 65535:
		return errors.New("port tidak valid")
	case s.Security != "starttls" && s.Security != "tls" && s.Security != "none":
		return errors.New("keamanan koneksi tidak dikenal")
	case s.Security == "none" && s.Username != "":
		return errors.New("login SMTP butuh koneksi terenkripsi (STARTTLS atau TLS)")
	case strings.ContainsAny(s.Username+s.Password, "\r\n\x00"):
		return errors.New("username atau password tidak valid")
	}
	return nil
}

// ParseAddress: satu alamat email polos (tanpa nama tampilan).
func ParseAddress(a string) (string, error) {
	a = strings.TrimSpace(a)
	if a == "" || len(a) > 254 || strings.ContainsAny(a, "\r\n\x00<>,; ") {
		return "", errors.New("alamat email tidak valid")
	}
	p, err := mail.ParseAddress(a)
	if err != nil || p.Address != a || p.Name != "" {
		return "", errors.New("alamat email tidak valid")
	}
	return a, nil
}

// Send mengirim m lewat s. Seluruh percakapan dibatasi ctx (dan paling lama
// satu menit).
func Send(ctx context.Context, s Server, m Message) error {
	if err := ValidServer(s); err != nil {
		return err
	}
	if _, err := ParseAddress(m.From); err != nil {
		return fmt.Errorf("pengirim: %w", err)
	}
	if len(m.To) == 0 {
		return errors.New("belum ada penerima")
	}
	for _, to := range m.To {
		if _, err := ParseAddress(to); err != nil {
			return fmt.Errorf("penerima %q: %w", to, err)
		}
	}
	msg, err := build(m, time.Now())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	host := strings.Trim(s.Host, "[]")
	addr := net.JoinHostPort(host, strconv.Itoa(s.Port))
	tc := s.tlsConfig
	if tc == nil {
		tc = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tc = tc.Clone()
	tc.ServerName = host

	d := &net.Dialer{}
	var conn net.Conn
	if s.Security == "tls" {
		conn, err = (&tls.Dialer{NetDialer: d, Config: tc}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("tidak bisa terhubung ke %s: %w", addr, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// Koneksi diputus bila ctx dibatalkan di tengah percakapan.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("server SMTP: %w", err)
	}
	defer c.Close()
	if err := c.Hello("sapo.local"); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}
	encrypted := s.Security == "tls"
	if s.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("server tidak mendukung STARTTLS; pilih TLS atau periksa port")
		}
		if err := c.StartTLS(tc); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
		encrypted = true
	}
	if s.Username != "" {
		if !encrypted {
			return errors.New("login SMTP butuh koneksi terenkripsi")
		}
		ok, mechs := c.Extension("AUTH")
		if !ok {
			return errors.New("server tidak meminta login; kosongkan username")
		}
		var a smtp.Auth
		switch {
		case hasMech(mechs, "PLAIN"):
			a = smtp.PlainAuth("", s.Username, s.Password, host)
		case hasMech(mechs, "LOGIN"):
			a = &loginAuth{user: s.Username, pass: s.Password}
		default:
			return fmt.Errorf("cara login server tidak didukung (%s)", mechs)
		}
		if err := c.Auth(a); err != nil {
			return fmt.Errorf("login SMTP ditolak: %w", err)
		}
	}
	if err := c.Mail(m.From); err != nil {
		return fmt.Errorf("pengirim ditolak: %w", err)
	}
	for _, to := range m.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("penerima %s ditolak: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("mengirim isi email: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email ditolak server: %w", err)
	}
	// Email sudah diterima server: galat saat QUIT tidak boleh membuatnya
	// dianggap gagal (dan dikirim ulang).
	_ = c.Quit()
	return nil
}

func hasMech(list, m string) bool {
	for _, x := range strings.Fields(list) {
		if strings.EqualFold(x, m) {
			return true
		}
	}
	return false
}

// build menyusun email: header tanpa input mentah (subjek dikodekan RFC
// 2047, alamat sudah diperiksa), isi UTF-8 quoted-printable.
func build(m Message, now time.Time) ([]byte, error) {
	if strings.ContainsAny(m.Subject, "\r\n") {
		return nil, errors.New("subjek tidak valid")
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := "sapo.local"
	if _, d, ok := strings.Cut(m.From, "@"); ok {
		domain = d
	}
	var b bytes.Buffer
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", m.From)
	h("To", strings.Join(m.To, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	h("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	body := strings.ReplaceAll(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n", "\r\n")
	if _, err := qp.Write([]byte(body)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// loginAuth: mekanisme AUTH LOGIN (dipakai sebagian server, misalnya
// Microsoft 365). Hanya dipakai setelah koneksi terenkripsi (diperiksa di
// Send).
type loginAuth struct{ user, pass string }

func (a *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }

func (a *loginAuth) Next(from []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(from))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("tantangan login tidak dikenal")
}
