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

// nfsOptions adalah opsi tetap setiap ekspor (selain ro/rw dan ID akun).
// secure: klien harus memakai port istimewa; sec=sys tanpa kepercayaan UID
// karena all_squash; no_subtree_check aman karena setiap folder adalah
// sistem berkas (dataset) tersendiri.
const nfsOptions = "sync,all_squash,secure,sec=sys,no_subtree_check"

// Panjang prefiks paling kecil untuk jaringan klien: jaringan yang lebih
// luas praktis berarti "semua orang".
const (
	minNFSPrefix4 = 8
	minNFSPrefix6 = 32
)

// validNFSClient: alamat atau jaringan dalam bentuk kanonis (bukan nama
// host, karena nama bisa dipalsukan lewat DNS).
func validNFSClient(c string) bool {
	p, err := netip.ParsePrefix(c)
	if err != nil || p.String() != c || p.Masked() != p {
		return false
	}
	if p.Addr().Is4() {
		return p.Bits() >= minNFSPrefix4
	}
	return p.Addr().Is6() && !p.Addr().Is4In6() && p.Bits() >= minNFSPrefix6 && p.Addr().Zone() == ""
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

// nfsAccount mencari UID dan GID akun NFS. Hanya akun buatan Sapo (rentang
// ID agent.accounts), jadi ekspor tidak pernah memetakan klien ke root atau
// akun sistem lain.
func (s *Shares) nfsAccount(ids shareIDs, name string) (int, int, error) {
	u, ok := ids.users[name]
	if !ok {
		return 0, 0, fmt.Errorf("akun NFS %s tidak ada", name)
	}
	if u.UID < s.t.IDMin || u.UID > s.t.IDMax || u.GID < s.t.IDMin || u.GID > s.t.IDMax {
		return 0, 0, fmt.Errorf("akun NFS %s bukan akun Sapo", name)
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
			fmt.Fprintf(&b, " %s(%s,%s,anonuid=%d,anongid=%d)", c, mode, nfsOptions, e.uid, e.gid)
		}
		b.WriteString("\n")
	}
	return b.Bytes()
}

// writeExports menulis file ekspor lalu meminta kernel memakainya
// (exportfs -ra). Server NFS hanya berjalan selama ada folder yang
// diekspor. Bila exportfs menolak, file lama dipulihkan. Hasil: "applied",
// atau "unavailable" bila server NFS tidak terpasang.
func (s *Shares) writeExports(ctx context.Context, exports []nfsConf) (string, error) {
	if _, err := os.Stat(s.t.Exportfs); err != nil {
		return "unavailable", nil
	}
	data := renderExports(exports)
	old, err := os.ReadFile(s.t.ExportsFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := s.replaceExports(data); err != nil {
		return "", err
	}
	if len(exports) > 0 {
		if err := s.nfsService(ctx, "start"); err != nil {
			return "", err
		}
	}
	if err := s.exportfs(ctx); err != nil {
		if old == nil {
			_ = os.Remove(s.t.ExportsFile)
		} else {
			_ = s.replaceExports(old)
		}
		_ = s.exportfs(ctx)
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

func (s *Shares) nfsService(ctx context.Context, action string) error {
	res, err := s.p.st.run(ctx, s.t.Systemctl, []string{action}, s.t.NFSService)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("systemctl %s %s: %s", action, s.t.NFSService, firstLine(res.Stderr))
	}
	return nil
}

// unexport mengeluarkan satu folder dari file ekspor (sebelum folder
// dilepas dan dihapus). File itu buatan sapod sendiri: satu baris per
// folder, diawali path folder.
func (s *Shares) unexport(ctx context.Context, path string) error {
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
		if first, _, _ := strings.Cut(line, " "); first == path {
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
