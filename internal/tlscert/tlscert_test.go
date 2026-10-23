package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func opts(t *testing.T, dir string, now time.Time, names ...string) Options {
	t.Helper()
	return Options{
		Dir: dir, Organization: "Sapo", DNSNames: names,
		IPs:      []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.168.1.10"), net.IPv4(127, 0, 0, 1)},
		Validity: 825 * 24 * time.Hour, RenewBefore: 30 * 24 * time.Hour, Now: func() time.Time { return now },
	}
}

func TestEnsureCreatesUsableCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	now := time.Now()
	c, err := Ensure(opts(t, dir, now, "NAS", "nas.local", "localhost"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Created {
		t.Fatal("sertifikat pertama harus ditandai baru")
	}
	leaf := c.Leaf
	if leaf.Subject.CommonName != "nas" || leaf.IsCA || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("subjek/penggunaan salah: %+v", leaf.Subject)
	}
	// Nama host dicocokkan tanpa membedakan huruf besar.
	for _, h := range []string{"nas", "nas.local", "localhost", "192.168.1.10"} {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("%s harus tercakup: %v", h, err)
		}
	}
	if len(leaf.IPAddresses) != 2 {
		t.Errorf("alamat IP ganda harus dibuang: %v", leaf.IPAddresses)
	}
	if !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(c.Fingerprint) {
		t.Fatalf("format sidik jari: %s", c.Fingerprint)
	}
	// Direktori dan kunci hanya bisa dibaca pemiliknya.
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, KeyFile): 0o600} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want {
			t.Errorf("izin %s = %v, harus %v", path, st.Mode().Perm(), want)
		}
	}
	// Pasangan sertifikat bisa dipakai server TLS sungguhan.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{c.TLS}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if conn, err := ln.Accept(); err == nil {
			_ = conn.(*tls.Conn).Handshake()
			conn.Close()
		}
	}()
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "nas.local"})
	if err != nil {
		t.Fatalf("handshake dengan sertifikat yang dipercaya harus berhasil: %v", err)
	}
	if got := Fingerprint(conn.ConnectionState().PeerCertificates[0].Raw); got != c.Fingerprint {
		t.Fatalf("sidik jari server %s, harus %s", got, c.Fingerprint)
	}
	conn.Close()
}

func TestEnsureReusesUntilRenewalNeeded(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	first, err := Ensure(opts(t, dir, start, "nas", "nas.local"))
	if err != nil {
		t.Fatal(err)
	}
	// Dimuat ulang: sidik jari sama, walau alamat IP mesin sudah berubah.
	o := opts(t, dir, start.Add(24*time.Hour), "nas", "nas.local")
	o.IPs = []net.IP{net.ParseIP("10.0.0.5")}
	again, err := Ensure(o)
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.Fingerprint != first.Fingerprint {
		t.Fatal("sertifikat yang masih layak tidak boleh diganti")
	}
	// Mendekati kedaluwarsa: diganti.
	late, err := Ensure(opts(t, dir, start.Add(800*24*time.Hour), "nas", "nas.local"))
	if err != nil {
		t.Fatal(err)
	}
	if !late.Created || late.Fingerprint == first.Fingerprint {
		t.Fatal("sertifikat yang hampir kedaluwarsa harus diganti")
	}
	// Nama host baru: diganti agar nama itu tercakup.
	renamed, err := Ensure(opts(t, dir, start.Add(801*24*time.Hour), "gudang", "gudang.local"))
	if err != nil {
		t.Fatal(err)
	}
	if !renamed.Created || renamed.Leaf.VerifyHostname("gudang.local") != nil {
		t.Fatal("ganti nama host harus membuat sertifikat baru")
	}
}

func TestEnsureReplacesBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CertFile), []byte("bukan sertifikat"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Ensure(opts(t, dir, time.Now(), "nas"))
	if err != nil || !c.Created {
		t.Fatalf("file rusak harus diganti: %v", err)
	}
}

func TestEnsureNeedsAName(t *testing.T) {
	if _, err := Ensure(opts(t, t.TempDir(), time.Now())); err == nil {
		t.Fatal("tanpa nama host harus gagal")
	}
}
