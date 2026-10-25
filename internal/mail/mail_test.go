package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP: server SMTP kecil untuk test. starttls: menawarkan STARTTLS
// (dan AUTH PLAIN setelahnya).
type fakeSMTP struct {
	ln       net.Listener
	starttls *tls.Config
	mu       sync.Mutex
	data     string
	auth     string
	sawTLS   bool
}

func newFake(t *testing.T, starttls *tls.Config) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, starttls: starttls}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r, w := bufio.NewReader(c), bufio.NewWriter(c)
	say := func(s string) { w.WriteString(s + "\r\n"); w.Flush() }
	say("220 uji")
	tlsOn := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.Fields(line + " x")[0])
		switch cmd {
		case "EHLO", "HELO":
			w.WriteString("250-uji\r\n")
			if f.starttls != nil && !tlsOn {
				w.WriteString("250-STARTTLS\r\n")
			}
			if tlsOn {
				w.WriteString("250-AUTH PLAIN\r\n")
			}
			say("250 OK")
		case "STARTTLS":
			say("220 mulai")
			tc := tls.Server(c, f.starttls)
			if tc.Handshake() != nil {
				return
			}
			c, tlsOn = tc, true
			r, w = bufio.NewReader(c), bufio.NewWriter(c)
			f.mu.Lock()
			f.sawTLS = true
			f.mu.Unlock()
		case "AUTH":
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.Fields(line)[2]))
			f.mu.Lock()
			f.auth = string(raw)
			f.mu.Unlock()
			say("235 OK")
		case "MAIL", "RCPT", "RSET", "NOOP":
			say("250 OK")
		case "DATA":
			say("354 lanjut")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			say("250 diterima")
		case "QUIT":
			say("221 sampai jumpa")
			return
		default:
			say("502 ?")
		}
	}
}

func testCert(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}

func TestSendPlain(t *testing.T) {
	f := newFake(t, nil)
	s := Server{Host: "127.0.0.1", Port: f.port(), Security: "none"}
	m := Message{From: "sapo@uji.local", To: []string{"admin@uji.local"}, Subject: "Pool tank tidak sehat ✓", Body: "Baris satu\nBaris dua"}
	if err := Send(context.Background(), s, m); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, want := range []string{"From: sapo@uji.local\r\n", "To: admin@uji.local\r\n", "Subject: =?utf-8?q?", "Baris satu\r\nBaris dua", "Auto-Submitted: auto-generated"} {
		if !strings.Contains(f.data, want) {
			t.Errorf("email tidak memuat %q:\n%s", want, f.data)
		}
	}
}

func TestSendStartTLSAndAuth(t *testing.T) {
	srv, pool := testCert(t)
	f := newFake(t, srv)
	s := Server{Host: "127.0.0.1", Port: f.port(), Security: "starttls", Username: "budi", Password: "rahasia", tlsConfig: &tls.Config{RootCAs: pool}}
	m := Message{From: "sapo@uji.local", To: []string{"admin@uji.local"}, Subject: "Uji", Body: "isi"}
	if err := Send(context.Background(), s, m); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if !f.sawTLS || f.auth != "\x00budi\x00rahasia" || !strings.Contains(f.data, "isi") {
		t.Fatalf("tls=%v auth=%q data=%q", f.sawTLS, f.auth, f.data)
	}
	f.mu.Unlock()
	// Sertifikat yang tidak dipercaya ditolak.
	s.tlsConfig = nil
	if err := Send(context.Background(), s, m); err == nil {
		t.Fatal("sertifikat tidak dipercaya harus ditolak")
	}
}

func TestSendRefusesDowngradeAndInjection(t *testing.T) {
	f := newFake(t, nil)
	m := Message{From: "sapo@uji.local", To: []string{"admin@uji.local"}, Subject: "Uji", Body: "isi"}
	// STARTTLS diminta tetapi server tidak menawarkannya.
	if err := Send(context.Background(), Server{Host: "127.0.0.1", Port: f.port(), Security: "starttls"}, m); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("tanpa STARTTLS: %v", err)
	}
	// Login tanpa enkripsi.
	if err := Send(context.Background(), Server{Host: "127.0.0.1", Port: f.port(), Security: "none", Username: "budi", Password: "x"}, m); err == nil {
		t.Fatal("login tanpa enkripsi harus ditolak")
	}
	for _, bad := range []string{"a@b.c\r\nBcc: x@y.z", "Nama <a@b.c>", "a@b.c, d@e.f", "", "bukan-alamat"} {
		if _, err := ParseAddress(bad); err == nil {
			t.Errorf("alamat %q harus ditolak", bad)
		}
	}
	m2 := m
	m2.Subject = "Uji\r\nBcc: x@y.z"
	if err := Send(context.Background(), Server{Host: "127.0.0.1", Port: f.port(), Security: "none"}, m2); err == nil {
		t.Fatal("subjek dengan baris baru harus ditolak")
	}
	for _, h := range []string{"smtp.gmail.com", "10.0.2.2", "[::1]", "mail-01.contoh.co.id"} {
		if err := ValidServer(Server{Host: h, Port: 587, Security: "starttls"}); err != nil {
			t.Errorf("%s: %v", h, err)
		}
	}
	for _, h := range []string{"", "a b", "-x", "a;b", "evil\r\nhost"} {
		if err := ValidServer(Server{Host: h, Port: 587, Security: "starttls"}); err == nil {
			t.Errorf("host %q harus ditolak", h)
		}
	}
}
