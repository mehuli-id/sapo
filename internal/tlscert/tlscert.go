// Package tlscert membuat dan memelihara sertifikat HTTPS self-signed untuk
// panel di jaringan lokal (ADR 0004). Sertifikat dibuat oleh sapo-api sendiri
// (tanpa root) di direktori datanya, dan dibuat ulang hanya bila hilang,
// rusak, mendekati kedaluwarsa, atau nama host tidak lagi tercakup, agar sidik
// jari yang sudah dicocokkan pengguna tidak berubah tanpa alasan.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Nama file di direktori sertifikat.
const (
	CertFile = "cert.pem"
	KeyFile  = "key.pem"
)

// Options mengatur sertifikat yang dibutuhkan.
type Options struct {
	Dir          string   // direktori penyimpanan (dibuat 0700)
	Organization string   // nama produk di subjek sertifikat
	DNSNames     []string // nama yang wajib tercakup, misalnya nas dan nas.local
	IPs          []net.IP // alamat saat sertifikat dibuat (tidak memicu pembuatan ulang)
	Validity     time.Duration
	RenewBefore  time.Duration
	Now          func() time.Time
}

// Cert adalah sertifikat yang siap dipakai beserta sidik jarinya.
type Cert struct {
	TLS         tls.Certificate
	Leaf        *x509.Certificate
	Fingerprint string // SHA-256 DER, heksadesimal huruf besar dipisah titik dua
	Created     bool   // true bila baru dibuat pada pemanggilan ini
}

// Ensure memuat sertifikat yang ada bila masih layak, atau membuat yang baru.
func Ensure(o Options) (Cert, error) {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return Cert{}, fmt.Errorf("membuat direktori sertifikat: %w", err)
	}
	if c, err := load(o.Dir); err == nil && usable(c.Leaf, o, now()) {
		return c, nil
	}
	return create(o, now())
}

func load(dir string) (Cert, error) {
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile))
	if err != nil {
		return Cert{}, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return Cert{}, err
	}
	pair.Leaf = leaf
	return Cert{TLS: pair, Leaf: leaf, Fingerprint: Fingerprint(leaf.Raw)}, nil
}

// usable: belum (hampir) kedaluwarsa dan mencakup semua nama yang wajib.
func usable(leaf *x509.Certificate, o Options, now time.Time) bool {
	if now.Before(leaf.NotBefore) || now.Add(o.RenewBefore).After(leaf.NotAfter) {
		return false
	}
	for _, n := range o.DNSNames {
		if !slices.Contains(leaf.DNSNames, strings.ToLower(n)) {
			return false
		}
	}
	return true
}

func create(o Options, now time.Time) (Cert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Cert{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Cert{}, err
	}
	names := make([]string, 0, len(o.DNSNames))
	for _, n := range o.DNSNames {
		if n = strings.ToLower(n); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return Cert{}, errors.New("sertifikat butuh minimal satu nama host")
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0], Organization: []string{o.Organization}},
		NotBefore:             now.Add(-time.Hour), // toleransi jam klien yang sedikit tertinggal
		NotAfter:              now.Add(o.Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           uniqueIPs(o.IPs),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return Cert{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Cert{}, err
	}
	// Kunci ditulis lebih dulu; keduanya lewat file sementara + rename agar
	// tidak pernah ada pasangan setengah jadi.
	if err := writeAtomic(filepath.Join(o.Dir, KeyFile), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})); err != nil {
		return Cert{}, err
	}
	if err := writeAtomic(filepath.Join(o.Dir, CertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return Cert{}, err
	}
	c, err := load(o.Dir)
	c.Created = true
	return c, err
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	return os.Rename(tmp.Name(), path)
}

// Fingerprint mengembalikan sidik jari SHA-256 sertifikat dalam format yang
// ditampilkan browser: AB:CD:…
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func uniqueIPs(ips []net.IP) []net.IP {
	out := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if !slices.ContainsFunc(out, ip.Equal) {
			out = append(out, ip)
		}
	}
	return out
}

// LocalIPs mengembalikan alamat IP antarmuka mesin ini (tanpa link-local),
// untuk dicantumkan di sertifikat saat dibuat.
func LocalIPs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLinkLocalUnicast() || n.IP.IsLinkLocalMulticast() {
			continue
		}
		out = append(out, n.IP)
	}
	return out
}
