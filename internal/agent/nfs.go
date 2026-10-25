package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Ekspor NFS shared folder (ADR 0007). File ekspor ditulis utuh oleh sapod
// dari templat tetap. Setiap akses dari klien dipetakan ke satu akun Sapo
// (all_squash, anonuid/anongid), jadi UID yang dikirim klien tidak pernah
// dipercaya dan izin NFS sama dengan izin akun itu di ACL folder.

// nfsOptions adalah opsi tetap setiap ekspor, setelah sec=sys dan ro/rw
// (opsi berlaku untuk sec= yang mendahuluinya). sec=sys tanpa kepercayaan
// UID karena all_squash; secure: klien harus memakai port istimewa;
// no_subtree_check aman karena setiap folder adalah sistem berkas (dataset)
// tersendiri, dan mp memastikan itu: folder yang belum terpasang (misalnya
// gagal dipasang saat boot) tidak diekspor sebagai direktori biasa di
// sistem berkas pool.
const nfsOptions = "sync,all_squash,secure,no_subtree_check,mp"

// nfsNetworks: klien NFS hanya boleh di jaringan lokal (privat IPv4, CGNAT
// yang dipakai VPN, IPv6 ULA). Jaringan yang lebih luas dari jaringan ini
// ditolak.
var nfsNetworks = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// validNFSClient: alamat atau jaringan lokal dalam bentuk kanonis (bukan
// nama host, karena nama bisa dipalsukan lewat DNS).
func validNFSClient(c string) bool {
	p, err := netip.ParsePrefix(c)
	if err != nil || p.String() != c || p.Masked() != p || p.Addr().Zone() != "" || p.Addr().Is4In6() {
		return false
	}
	return inNFSNetworks(p)
}

// checkNFSDeny: klien NFS tidak boleh bertumpuk dengan jaringan yang
// ditolak (jaringan container App Center).
func (s *Shares) checkNFSDeny(sp proto.ShareSpec) error {
	if sp.NFS == nil {
		return nil
	}
	for _, c := range sp.NFS.Clients {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			continue
		}
		for _, d := range s.t.DenyNetworks {
			if dp, err := netip.ParsePrefix(d); err == nil && dp.Overlaps(p) {
				return fmt.Errorf("klien NFS %s bertumpuk dengan jaringan aplikasi %s", c, d)
			}
		}
	}
	return nil
}

func inNFSNetworks(p netip.Prefix) bool {
	for _, n := range nfsNetworks {
		if n.Bits() <= p.Bits() && n.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

func checkNFSSpec(sp proto.ShareSpec) error {
	n := sp.NFS
	if n == nil {
		return nil
	}
	if len(n.Clients) == 0 || len(n.Clients) > proto.MaxNFSClients || !accountNamePattern.MatchString(n.User) {
		return fmt.Errorf("ekspor NFS %s tidak valid", sp.Name)
	}
	seen := map[string]bool{}
	for _, c := range n.Clients {
		if !validNFSClient(c) || seen[c] {
			return fmt.Errorf("klien NFS tidak valid di %s: %s", sp.Name, clip(c))
		}
		seen[c] = true
	}
	return nil
}

// withNFSUser menambahkan akun NFS ke izin folder. Akses lewat NFS hanya
// membawa UID dan grup utama akun itu (grup tambahan dibuang oleh
// all_squash), jadi akun itu perlu entri ACL sendiri walau izinnya berasal
// dari grup.
func withNFSUser(sp proto.ShareSpec) proto.ShareSpec {
	if sp.NFS == nil {
		return sp
	}
	p := "user:" + sp.NFS.User
	if slices.Contains(sp.Write, p) || (sp.NFS.ReadOnly && slices.Contains(sp.Read, p)) {
		return sp
	}
	if sp.NFS.ReadOnly {
		sp.Read = append(slices.Clone(sp.Read), p)
	} else {
		sp.Read = slices.DeleteFunc(slices.Clone(sp.Read), func(x string) bool { return x == p })
		sp.Write = append(slices.Clone(sp.Write), p)
	}
	return sp
}

type nfsConf struct {
	path     string
	clients  []string
	uid, gid int
	readOnly bool
}

// nfsAccount mencari UID dan GID akun NFS. Hanya akun buatan Sapo (di
// rentang ID agent.accounts dan tercatat di catatan akun sapod), jadi
// ekspor tidak pernah memetakan klien ke root atau akun sistem lain.
func (s *Shares) nfsAccount(ids shareIDs, name string) (int, int, error) {
	u, ok := ids.users[name]
	if !ok {
		return 0, 0, fmt.Errorf("akun NFS %s tidak ada", name)
	}
	if u.UID < s.t.IDMin || u.UID > s.t.IDMax || u.GID < s.t.IDMin || u.GID > s.t.IDMax {
		return 0, 0, fmt.Errorf("akun NFS %s bukan akun Sapo", name)
	}
	if s.managed != nil {
		uid, gid, ok := s.managed(name)
		if !ok || uid != u.UID || gid != u.GID {
			return 0, 0, fmt.Errorf("akun NFS %s bukan akun Sapo", name)
		}
	}
	return u.UID, u.GID, nil
}

// nfsPathPattern: path folder yang boleh masuk ke file ekspor (tanpa spasi,
// kutip, atau karakter lain yang punya arti di exports(5)).
var nfsPathPattern = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)

// renderExports menyusun file ekspor. Path dan klien sudah divalidasi.
func renderExports(exports []nfsConf) []byte {
	var b bytes.Buffer
	b.WriteString("# Dibuat oleh sapod (Sapo). Jangan diubah manual: atur NFS shared folder lewat panel.\n")
	for _, e := range exports {
		mode := "rw"
		if e.readOnly {
			mode = "ro"
		}
		b.WriteString(e.path)
		for _, c := range e.clients {
			fmt.Fprintf(&b, " %s(sec=sys,%s,%s,anonuid=%d,anongid=%d)", c, mode, nfsOptions, e.uid, e.gid)
		}
		b.WriteString("\n")
	}
	return b.Bytes()
}

// writeExports menulis file ekspor lalu meminta kernel memakainya
// (exportfs -ra). Server NFS hanya berjalan (dan menyala saat boot) selama
// ada folder yang diekspor. Bila gagal, semua ekspor Sapo dicabut
// (gagal-tertutup): file lama bisa saja memberi akses yang baru dicabut.
// Hasil: "applied", atau "unavailable" bila server NFS tidak terpasang.
func (s *Shares) writeExports(ctx context.Context, exports []nfsConf) (string, error) {
	if _, err := os.Stat(s.t.Exportfs); err != nil {
		return "unavailable", nil
	}
	err := s.replaceExports(renderExports(exports))
	if err == nil && len(exports) > 0 {
		err = s.nfsService(ctx, "start")
	}
	if err == nil {
		err = s.exportfs(ctx)
	}
	if err != nil {
		s.p.logWarn("ekspor NFS gagal diterapkan; semua ekspor Sapo dicabut", "err", err)
		if rmErr := s.replaceExports(renderExports(nil)); rmErr != nil {
			_ = os.Remove(s.t.ExportsFile)
		}
		_ = s.exportfs(ctx)
		_ = s.nfsService(ctx, "stop")
		return "", err
	}
	if len(exports) == 0 {
		if err := s.nfsService(ctx, "stop"); err != nil {
			return "", err
		}
	}
	return "applied", nil
}

func (s *Shares) replaceExports(data []byte) error {
	dir := filepath.Dir(s.t.ExportsFile)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := checkSocketDir(dir); err != nil {
		return fmt.Errorf("direktori ekspor NFS tidak aman: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".sapo-exports-*")
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
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.t.ExportsFile)
}

func (s *Shares) exportfs(ctx context.Context) error {
	res, err := s.p.st.run(ctx, s.t.Exportfs, []string{"-ra"})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exportfs: %s", clipTo(strings.TrimSpace(string(res.Stderr)), maxErrorText))
	}
	return nil
}

// nfsService menyalakan (start) atau mematikan (stop) server NFS, sekaligus
// menyalakan atau tidak saat boot. Hanya dijalankan bila keadaannya berubah
// (sejak sapod menyala), agar shares.apply tetap cepat.
func (s *Shares) nfsService(ctx context.Context, action string) error {
	if s.nfsState == action {
		return nil
	}
	s.nfsState = ""
	flags := []string{"enable", "--now"}
	if action == "stop" {
		flags = []string{"disable", "--now"}
	}
	res, err := s.p.st.run(ctx, s.t.Systemctl, flags, s.t.NFSService)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("systemctl %s %s: %s", action, s.t.NFSService, firstLine(res.Stderr))
	}
	s.nfsState = action
	return nil
}

// unexport mengeluarkan folder dengan path yang cocok dari file ekspor
// (sebelum folder atau pool dilepas dan dihapus). File itu buatan sapod
// sendiri: satu baris per folder, diawali path folder.
func (s *Shares) unexport(ctx context.Context, match func(path string) bool) error {
	if _, err := os.Stat(s.t.Exportfs); err != nil {
		return nil
	}
	old, err := os.ReadFile(s.t.ExportsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var b bytes.Buffer
	found, remaining := false, 0
	sc := bufio.NewScanner(bytes.NewReader(old))
	for sc.Scan() {
		line := sc.Text()
		if first, _, _ := strings.Cut(line, " "); strings.HasPrefix(first, "/") && match(first) {
			found = true
			continue
		}
		if line != "" && !strings.HasPrefix(line, "#") {
			remaining++
		}
		b.WriteString(line + "\n")
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := s.replaceExports(b.Bytes()); err != nil {
		return err
	}
	if err := s.exportfs(ctx); err != nil {
		return err
	}
	if remaining == 0 {
		return s.nfsService(ctx, "stop")
	}
	return nil
}

// unexportPool mengeluarkan semua folder di pool dari file ekspor, sebelum
// pool dihapus.
func (s *Shares) unexportPool(ctx context.Context, pool string) error {
	base := filepath.Join(s.t.MountBase, pool) + "/"
	return s.unexport(ctx, func(path string) bool { return strings.HasPrefix(path, base) })
}
