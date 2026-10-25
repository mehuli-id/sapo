package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/proto"
)

// ShareTools adalah program dan nilai Samba untuk shared folder
// (agent.shares di sapo.yaml).
type ShareTools struct {
	Zfs, Setfacl, Testparm, Smbcontrol string
	SMBConf                            string
	Workgroup, ServerString            string
	MinProtocol                        string
	// MountBase: pool Sapo terpasang di <MountBase>/<pool>; folder harus
	// tepat di <MountBase>/<pool>/<nama>.
	MountBase string
	// Untuk mengubah nama pengguna dan grup menjadi ID (ACL isi folder).
	PasswdFile, GroupFile string
	// NFS: file ekspor yang ditulis utuh oleh sapod, exportfs, dan layanan
	// server NFS (dijalankan hanya selama ada folder yang diekspor).
	Exportfs, Systemctl, ExportsFile, NFSService string
	// DenyNetworks: jaringan yang tidak pernah boleh memakai SMB atau NFS
	// (jaringan container App Center).
	DenyNetworks []string
	// SMBService: layanan Samba, dimatikan bila SMB dimatikan di Control
	// Panel.
	SMBService string
	// Rentang ID akun Sapo (agent.accounts): klien NFS hanya boleh
	// dipetakan ke akun di rentang ini.
	IDMin, IDMax int
}

// Shares melayani shares.apply, shares.delete, dan shares.usage. Perubahan
// memakai kunci yang sama dengan operasi pool, agar folder tidak dibuat atau
// dihapus saat pool sedang dibuat, diganti disknya, atau dihapus.
type Shares struct {
	p *Pools
	t ShareTools
	// chown dan isMount diganti di test (yang tidak berjalan sebagai root
	// dan tanpa ZFS).
	chown   func(path string, uid, gid int) error
	isMount func(path string) bool
	// aclTree memasang ACL ke isi folder (diganti di test).
	aclTree func(ctx context.Context, root string, acl shareACL) error
	// managed: UID dan GID akun buatan sapod (Accounts.Managed); nil di test.
	managed func(name string) (uid, gid int, ok bool)
	// apps: App Center (nil bila tidak dipakai), untuk akses aplikasi ke
	// folder.
	apps *Apps

	jobsMu sync.Mutex
	jobs   map[string]*aclJob
	// nfsState: keadaan server NFS terakhir yang diterapkan ("start",
	// "stop", atau "" = belum diketahui). Dilindungi kunci pool.
	nfsState string
	smbState string
}

// aclJob adalah penerapan izin ke isi satu folder yang berjalan di latar
// belakang.
type aclJob struct {
	hash     string
	cancel   context.CancelFunc
	finished chan struct{}
	done     bool
	err      string
}

// NewShares menyiapkan operasi shared folder di atas pool p.
func NewShares(p *Pools, t ShareTools) *Shares {
	s := &Shares{p: p, t: t, chown: os.Lchown, isMount: isMountPoint, aclTree: applyACLTree, jobs: map[string]*aclJob{}}
	// Ekspor NFS folder di pool dicabut sebelum pool dihapus.
	p.beforeDestroy = append(p.beforeDestroy, s.unexportPool)
	return s
}

// SetAccounts menghubungkan catatan akun sapod: klien NFS hanya dipetakan
// ke akun yang tercatat dibuat sapod.
func (s *Shares) SetAccounts(a *Accounts) { s.managed = a.Managed }

// isMountPoint: path berada di perangkat yang berbeda dengan induknya.
func isMountPoint(path string) bool {
	var self, parent syscall.Stat_t
	if syscall.Lstat(path, &self) != nil || syscall.Lstat(filepath.Dir(path), &parent) != nil {
		return false
	}
	return self.Dev != parent.Dev
}

// Penanda dataset buatan Sapo (properti pengguna ZFS). Dataset tanpa
// penanda tidak pernah diubah izinnya, dibagikan, atau dihapus.
const (
	sharePropName = "sapo:share"
	sharePropACL  = "sapo:acl"
)

// shareNamePattern: nama folder (juga nama share SMB dan nama dataset).
var shareNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Nama share yang punya arti khusus di Samba.
var reservedShares = []string{"global", "homes", "printers", "print", "ipc", "sapo-apps"}

// validShareText: keterangan folder masuk ke smb.conf (comment = …), jadi
// hanya teks satu baris tanpa karakter kendali, format, atau tanda kurung siku.
func validShareText(s string) bool {
	if utf8.RuneCountInString(s) > 64 || !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '[' || r == ']' || r == '\\' || r == '%'
	})
}

// principal memeriksa "user:nama" atau "group:nama".
func principal(p string) (kind, name string, ok bool) {
	kind, name, found := strings.Cut(p, ":")
	if !found || (kind != "user" && kind != "group") || !accountNamePattern.MatchString(name) {
		return "", "", false
	}
	return kind, name, true
}

func (s *Shares) checkSpecs(specs []proto.ShareSpec) error {
	invalid := func(msg string) error { return &OpError{Code: proto.ErrInvalidParams, Message: msg} }
	if len(specs) > 1000 {
		return invalid("terlalu banyak shared folder")
	}
	seen := map[string]bool{}
	for _, sp := range specs {
		if !shareNamePattern.MatchString(sp.Name) || slices.Contains(reservedShares, sp.Name) || seen[sp.Name] {
			return invalid("nama shared folder tidak valid: " + clip(sp.Name))
		}
		seen[sp.Name] = true
		if !poolNamePattern.MatchString(sp.Pool) || !validShareText(sp.Description) {
			return invalid("pool atau keterangan shared folder " + sp.Name + " tidak valid")
		}
		if len(sp.Read)+len(sp.Write) > 1000 {
			return invalid("terlalu banyak izin di " + sp.Name)
		}
		for _, p := range append(slices.Clone(sp.Read), sp.Write...) {
			// Akun aplikasi hanya lewat Apps (tidak pernah masuk Samba).
			if _, _, ok := principal(p); !ok || strings.HasPrefix(p, "user:sapo-app-") || strings.HasPrefix(p, "group:sapo-app-") {
				return invalid("izin tidak valid di " + sp.Name + ": " + clip(p))
			}
		}
		if len(sp.Apps) > 100 {
			return invalid("terlalu banyak aplikasi di " + sp.Name)
		}
		apps := map[string]bool{}
		for _, ap := range sp.Apps {
			if !appIDPattern.MatchString(ap.ID) || apps[ap.ID] {
				return invalid("aplikasi tidak valid di " + sp.Name + ": " + clip(ap.ID))
			}
			apps[ap.ID] = true
		}
		if err := s.checkNFSDeny(sp); err != nil {
			return invalid(err.Error())
		}
		if err := checkNFSSpec(sp); err != nil {
			return invalid(err.Error())
		}
	}
	return nil
}

// ApplyOp mengembalikan handler shares.apply: membuat dataset yang belum
// ada, memasang izin folder, lalu menulis ulang smb.conf untuk semua folder.
// Folder yang tidak ada di daftar TIDAK dihapus (penghapusan hanya lewat
// shares.delete); folder itu hanya tidak lagi dibagikan.
func (s *Shares) ApplyOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.SharesApplyParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		if err := s.checkSpecs(req.Shares); err != nil {
			return nil, err
		}
		if !s.p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer s.p.write.Unlock()
		ctx, cancel := context.WithTimeout(ctx, s.p.t.CreateTimeout)
		defer cancel()

		pools, err := s.p.poolNames(ctx)
		if err != nil {
			return nil, err
		}
		ids, err := s.lookupIDs()
		if err != nil {
			return nil, err
		}
		// Aplikasi yang bisa diberi folder (terpasang, memakai share_path).
		var appAccess map[string]bool
		if s.apps != nil {
			appAccess = s.apps.shareAccess()
		}
		// Akun aplikasi yang hilang dari sistem tidak membuat folder gagal.
		for id := range appAccess {
			if _, ok := ids.users[appAccount(id)]; !ok {
				delete(appAccess, id)
			}
		}
		appMounts := map[string][]appShare{}
		if s.apps != nil {
			// Rencana (path folder belum diperiksa): aplikasi yang akan
			// kehilangan akses dihentikan sebelum ACL diubah. Bila gagal,
			// tetap dilanjutkan; pencabutan di sistem berkas tetap berlaku.
			planned := map[string][]appShare{}
			for _, sp := range req.Shares {
				_, apps := withApps(sp, appAccess)
				for _, ap := range apps {
					planned[ap.ID] = append(planned[ap.ID], appShare{Name: sp.Name, Path: filepath.Join(s.t.MountBase, sp.Pool, sp.Name), Write: ap.Write})
				}
			}
			if err := s.apps.stopReduced(ctx, planned); err != nil {
				s.p.logWarn("aplikasi gagal dihentikan sebelum akses folder dicabut", "err", err)
			}
		}
		out := proto.SharesApplyResult{Shares: []proto.ShareState{}}
		var active []shareConf
		var exports []nfsConf
		for _, sp := range req.Shares {
			if !pools[sp.Pool] {
				out.Shares = append(out.Shares, proto.ShareState{Name: sp.Name, Pool: sp.Pool, Error: "pool tidak ditemukan"})
				continue
			}
			st := proto.ShareState{Name: sp.Name, Pool: sp.Pool}
			var nfs *nfsConf
			if sp.NFS != nil {
				uid, gid, err := s.nfsAccount(ids, sp.NFS.User)
				if err != nil {
					// Folder tetap disiapkan untuk SMB, tanpa ekspor NFS.
					st.NFSError = err.Error()
					sp.NFS = nil
				} else {
					nfs = &nfsConf{clients: sp.NFS.Clients, uid: uid, gid: gid, readOnly: sp.NFS.ReadOnly}
				}
			}
			// Izin di sistem berkas mencakup akun NFS; Samba memakai izin asli.
			perms := withNFSUser(sp)
			// Demikian juga akun aplikasi yang diberi folder ini.
			perms, apps := withApps(perms, appAccess)
			acl, aclErr := ids.resolve(perms)
			path, applied, err := s.ensureDataset(ctx, sp)
			if err == nil {
				err = aclErr
			}
			if err == nil {
				err = s.setPermissions(ctx, path, perms)
			}
			st.Path = path
			if err != nil {
				st.Error = clipTo(err.Error(), maxErrorText)
				s.p.logWarn("shared folder gagal disiapkan", "folder", sp.Name, "err", err)
			} else {
				// Izin juga diterapkan ke isi folder yang sudah ada (latar
				// belakang), bila berbeda dari yang terakhir diterapkan.
				s.startACLJob(sp.Pool+"/"+sp.Name, path, acl, applied)
				for _, ap := range apps {
					appMounts[ap.ID] = append(appMounts[ap.ID], appShare{Name: sp.Name, Path: path, Write: ap.Write})
				}
				if len(sp.Read)+len(sp.Write) > 0 {
					// Folder tanpa izin sama sekali tidak dibagikan (valid
					// users kosong di Samba berarti semua pengguna boleh masuk).
					active = append(active, shareConf{spec: sp, path: path})
				}
				if nfs != nil {
					if nfsPathPattern.MatchString(path) {
						nfs.path = path
						exports = append(exports, *nfs)
					} else {
						st.NFSError = "path folder tidak bisa diekspor lewat NFS"
					}
				}
			}
			out.Shares = append(out.Shares, st)
		}
		// NFS dulu dan selalu, agar pencabutan akses NFS tidak tertahan oleh
		// galat Samba.
		// Layanan yang dimatikan di Control Panel tidak membagikan apa pun.
		if req.NFSOff {
			exports = nil
		}
		if req.SMBOff {
			active = nil
		}
		var nfsErr, smbErr, appErr error
		out.NFS, nfsErr = s.writeExports(ctx, exports)
		out.SMB, smbErr = s.writeSMBConf(ctx, active)
		// Folder aplikasi: keadaan lengkap, jadi folder yang dicabut (atau
		// gagal disiapkan) dilepas dari container.
		if s.apps != nil {
			appErr = s.apps.applyShares(ctx, appMounts)
		}
		switch {
		case req.SMBOff && out.SMB != "unavailable":
			// Gagal-tertutup: Samba dimatikan walau smb.conf gagal ditulis.
			smbErr = errors.Join(smbErr, s.smbService(ctx, true))
		case smbErr == nil && out.SMB == "applied":
			smbErr = s.smbService(ctx, false)
		}
		if err := errors.Join(nfsErr, smbErr, appErr); err != nil {
			return nil, err
		}
		return out, nil
	}
}

// smbService menyalakan atau mematikan Samba (termasuk saat boot), hanya
// bila keadaannya berubah sejak sapod menyala.
func (s *Shares) smbService(ctx context.Context, off bool) error {
	if s.t.SMBService == "" {
		return nil
	}
	flags, state := []string{"enable", "--now"}, "start"
	if off {
		flags, state = []string{"disable", "--now"}, "stop"
	}
	// Mematikan selalu dijalankan (murah, dan memperbaiki Samba yang
	// dinyalakan di luar Sapo); menyalakan hanya bila keadaannya berubah.
	if !off && s.smbState == state {
		return nil
	}
	res, err := s.p.st.run(ctx, s.t.Systemctl, flags, s.t.SMBService)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("systemctl %s %s: %s", strings.Join(flags, " "), s.t.SMBService, firstLine(res.Stderr))
	}
	s.smbState = state
	return nil
}

// ensureDataset membuat <pool>/<nama> bila belum ada (dengan penanda
// sapo:share), lalu memastikan dataset itu memang folder Sapo yang terpasang
// tepat di <MountBase>/<pool>/<nama>. Mengembalikan path dan nilai sapo:acl.
func (s *Shares) ensureDataset(ctx context.Context, sp proto.ShareSpec) (string, string, error) {
	ds := sp.Pool + "/" + sp.Name
	want := filepath.Join(s.t.MountBase, sp.Pool, sp.Name)
	// Akar pool harus terpasang di tempat Sapo (pool dari luar Sapo, misalnya
	// sistem root-on-ZFS, tidak dipakai untuk shared folder).
	props, err := s.zfsProps(ctx, sp.Pool, "mountpoint")
	if err != nil {
		return "", "", err
	}
	if props["mountpoint"] != filepath.Join(s.t.MountBase, sp.Pool) {
		return "", "", fmt.Errorf("pool %s tidak terpasang di %s", sp.Pool, filepath.Join(s.t.MountBase, sp.Pool))
	}
	props, err = s.zfsProps(ctx, ds, "mountpoint", "mounted", sharePropName, sharePropACL)
	if errors.Is(err, errNoDataset) {
		// devices/setuid off: berkas perangkat dan setuid di folder bersama
		// tidak pernah berlaku.
		res, runErr := s.p.st.run(ctx, s.t.Zfs, []string{"create", "-o", sharePropName + "=" + sp.Name, "-o", "devices=off", "-o", "setuid=off"}, ds)
		if runErr != nil {
			return "", "", runErr
		}
		if res.ExitCode != 0 {
			return "", "", fmt.Errorf("zfs create: %s", firstLine(res.Stderr))
		}
		s.p.logInfo("dataset shared folder dibuat", "dataset", ds)
		props, err = s.zfsProps(ctx, ds, "mountpoint", "mounted", sharePropName, sharePropACL)
	}
	if err != nil {
		return "", "", err
	}
	switch {
	case props[sharePropName] != sp.Name:
		return "", "", fmt.Errorf("dataset %s sudah ada dan bukan shared folder Sapo", ds)
	case props["mountpoint"] != want:
		return "", "", fmt.Errorf("dataset %s terpasang di %q, bukan %s", ds, clip(props["mountpoint"]), want)
	case props["mounted"] != "yes":
		return "", "", fmt.Errorf("dataset %s belum terpasang", ds)
	}
	// Harus direktori sungguhan (bukan symlink) yang benar-benar titik
	// pasang dataset itu, bukan direktori biasa di bawah pool.
	if st, err := os.Lstat(want); err != nil || !st.IsDir() || !s.isMount(want) {
		return "", "", fmt.Errorf("%s belum terpasang di %s", ds, want)
	}
	acl := props[sharePropACL]
	if acl == "-" {
		acl = ""
	}
	return want, acl, nil
}

var errNoDataset = errors.New("dataset tidak ada")

// zfsProps membaca properti dataset (zfs get -H -p). errNoDataset bila
// dataset tidak ada.
func (s *Shares) zfsProps(ctx context.Context, ds string, names ...string) (map[string]string, error) {
	res, err := s.p.st.run(ctx, s.t.Zfs, []string{"get", "-H", "-p", "-o", "property,value", strings.Join(names, ",")}, ds)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		if strings.Contains(string(res.Stderr), "does not exist") {
			return nil, errNoDataset
		}
		return nil, fmt.Errorf("zfs get %s: %s", ds, firstLine(res.Stderr))
	}
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "\t"); ok {
			out[k] = v
		}
	}
	return out, nil
}

// shareIDs memetakan nama akun dan grup ke ID (dari /etc/passwd dan
// /etc/group) untuk ACL isi folder.
type shareIDs struct {
	users  map[string]passwdEntry
	groups map[string]groupEntry
}

func (s *Shares) lookupIDs() (shareIDs, error) {
	users, err := readPasswd(s.t.PasswdFile)
	if err != nil {
		return shareIDs{}, err
	}
	groups, err := readGroup(s.t.GroupFile)
	if err != nil {
		return shareIDs{}, err
	}
	return shareIDs{users, groups}, nil
}

func (ids shareIDs) resolve(sp proto.ShareSpec) (shareACL, error) {
	var acl shareACL
	add := func(list []string, write bool) error {
		for _, p := range list {
			kind, name, _ := principal(p)
			switch kind {
			case "user":
				u, ok := ids.users[name]
				if !ok {
					return fmt.Errorf("akun %s tidak ada", name)
				}
				if write {
					acl.WriteUsers = append(acl.WriteUsers, uint32(u.UID))
				} else {
					acl.ReadUsers = append(acl.ReadUsers, uint32(u.UID))
				}
			case "group":
				g, ok := ids.groups[name]
				if !ok {
					return fmt.Errorf("grup %s tidak ada", name)
				}
				if write {
					acl.WriteGroups = append(acl.WriteGroups, uint32(g.GID))
				} else {
					acl.ReadGroups = append(acl.ReadGroups, uint32(g.GID))
				}
			}
		}
		return nil
	}
	if err := add(sp.Read, false); err != nil {
		return acl, err
	}
	return acl, add(sp.Write, true)
}

// aclHash meringkas ACL (dengan ID numerik) untuk properti sapo:acl.
func aclHash(acl shareACL) string {
	sum := sha256.Sum256(encodeACL(acl.entries(true)))
	return hex.EncodeToString(sum[:12])
}

// startACLJob menerapkan ACL ke isi folder di latar belakang bila hash-nya
// berbeda dari yang tercatat di dataset (sapo:acl). Pekerjaan lama untuk
// folder yang sama dibatalkan.
func (s *Shares) startACLJob(ds, path string, acl shareACL, applied string) {
	hash := aclHash(acl)
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	prev := s.jobs[ds]
	// Yang tercatat di dataset adalah sumber kebenaran; catatan di memori
	// hanya menandai pekerjaan yang sedang berjalan.
	if applied == hash {
		if prev != nil {
			prev.cancel()
		}
		delete(s.jobs, ds)
		return
	}
	if prev != nil && prev.hash == hash && !prev.done {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	job := &aclJob{hash: hash, cancel: cancel, finished: make(chan struct{})}
	s.jobs[ds] = job
	if prev != nil {
		prev.cancel()
	}
	go func() {
		defer close(job.finished)
		defer cancel()
		// Pekerjaan lama harus benar-benar berhenti dulu, agar tidak menulis
		// ACL lama setelah ACL baru.
		if prev != nil {
			<-prev.finished
		}
		err := ctx.Err()
		if err == nil {
			err = s.aclTree(ctx, path, acl)
		}
		if err == nil {
			res, runErr := runTool(ctx, s.p.st.t.Timeout, s.t.Zfs, []string{"set", sharePropACL + "=" + hash}, ds)
			switch {
			case runErr != nil:
				err = runErr
			case res.ExitCode != 0:
				err = errors.New("zfs set: " + firstLine(res.Stderr))
			}
		}
		s.jobsMu.Lock()
		defer s.jobsMu.Unlock()
		job.done = true
		if err != nil && ctx.Err() == nil {
			job.err = clipTo(err.Error(), maxErrorText)
			s.p.logWarn("izin isi folder gagal diterapkan", "dataset", ds, "err", err)
		} else if err == nil {
			s.p.logInfo("izin isi folder diterapkan", "dataset", ds)
		}
	}()
}

// stopACLJob membatalkan dan menunggu pekerjaan ACL folder (sebelum folder
// dihapus, agar tidak ada berkas yang masih terbuka).
func (s *Shares) stopACLJob(ds string) {
	s.jobsMu.Lock()
	j := s.jobs[ds]
	delete(s.jobs, ds)
	s.jobsMu.Unlock()
	if j == nil {
		return
	}
	j.cancel()
	select {
	case <-j.finished:
	case <-time.After(30 * time.Second):
	}
}

// setPermissions: pemilik root, grup akun Sapo, mode 0770, dan ACL POSIX:
// izin tulis = rwx, izin baca = r-x, pengguna lain tanpa akses. ACL bawaan
// (default) membuat file baru di dalamnya mewarisi izin yang sama.
func (s *Shares) setPermissions(ctx context.Context, path string, sp proto.ShareSpec) error {
	// Lchown tidak mengikuti symlink; path sudah dipastikan direktori.
	if err := s.chown(path, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o770); err != nil {
		return err
	}
	entries := []string{"u::rwx", "g::---", "o::---", "m::rwx"}
	add := func(list []string, perm string) {
		for _, p := range list {
			kind, name, _ := principal(p)
			tag := map[string]string{"user": "u", "group": "g"}[kind]
			entries = append(entries, tag+":"+name+":"+perm)
		}
	}
	add(sp.Read, "r-x")
	add(sp.Write, "rwx")
	acl := entries
	for _, e := range entries {
		acl = append(acl, "d:"+e)
	}
	res, err := s.p.st.run(ctx, s.t.Setfacl, []string{"--physical", "--set", strings.Join(acl, ",")}, path)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("setfacl: %s", firstLine(res.Stderr))
	}
	return nil
}

type shareConf struct {
	spec proto.ShareSpec
	path string
}

// renderSMBConf menyusun smb.conf dari templat tetap. Semua nilai sudah
// divalidasi (nama, path, keterangan), jadi tidak bisa menambah baris atau
// bagian baru.
func (s *Shares) renderSMBConf(shares []shareConf) []byte {
	var b bytes.Buffer
	b.WriteString("# Dibuat oleh sapod (Sapo). Jangan diubah manual: atur shared folder lewat panel.\n")
	fmt.Fprintf(&b, "[global]\n")
	for _, kv := range [][2]string{
		{"workgroup", s.t.Workgroup},
		{"server string", s.t.ServerString},
		{"server role", "standalone server"},
		{"server min protocol", s.t.MinProtocol},
		{"security", "user"},
		{"map to guest", "never"},
		{"restrict anonymous", "2"},
		{"usershare max shares", "0"},
		{"load printers", "no"},
		{"printing", "bsd"},
		{"printcap name", "/dev/null"},
		{"disable spoolss", "yes"},
		{"unix password sync", "no"},
		{"invalid users", "root"},
		{"hosts deny", strings.Join(s.t.DenyNetworks, " ")},
		{"logging", "file"},
		{"log file", "/var/log/samba/log.%m"},
		{"max log size", "1000"},
	} {
		fmt.Fprintf(&b, "   %s = %s\n", kv[0], kv[1])
	}
	for _, sc := range shares {
		var valid, write []string
		name := func(p string) string {
			kind, n, _ := principal(p)
			if kind == "group" {
				return "+" + n // grup Unix
			}
			return n
		}
		for _, p := range sc.spec.Read {
			valid = append(valid, name(p))
		}
		for _, p := range sc.spec.Write {
			valid = append(valid, name(p))
			write = append(write, name(p))
		}
		fmt.Fprintf(&b, "\n[%s]\n", sc.spec.Name)
		if sc.spec.Description != "" {
			fmt.Fprintf(&b, "   comment = %s\n", sc.spec.Description)
		}
		fmt.Fprintf(&b, "   path = %s\n", sc.path)
		b.WriteString("   browseable = yes\n   guest ok = no\n   read only = yes\n   inherit acls = yes\n")
		fmt.Fprintf(&b, "   valid users = %s\n", strings.Join(valid, " "))
		if len(write) > 0 {
			fmt.Fprintf(&b, "   write list = %s\n", strings.Join(write, " "))
		}
	}
	return b.Bytes()
}

// writeSMBConf menulis smb.conf (diperiksa testparm dulu) lalu meminta Samba
// memuat ulang. Hasil: "applied", atau "unavailable" bila Samba tidak
// terpasang.
func (s *Shares) writeSMBConf(ctx context.Context, shares []shareConf) (string, error) {
	if _, err := os.Stat(s.t.Testparm); err != nil {
		return "unavailable", nil
	}
	data := s.renderSMBConf(shares)
	dir := filepath.Dir(s.t.SMBConf)
	if err := checkSocketDir(dir); err != nil {
		return "", fmt.Errorf("direktori smb.conf tidak aman: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".smb.conf-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	res, err := s.p.st.run(ctx, s.t.Testparm, []string{"--suppress-prompt", "-s"}, tmp.Name())
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("testparm menolak smb.conf: %s", firstLine(res.Stderr))
	}
	old, _ := os.ReadFile(s.t.SMBConf)
	if err := os.Rename(tmp.Name(), s.t.SMBConf); err != nil {
		return "", err
	}
	// smbd yang belum berjalan tidak perlu dimuat ulang.
	_, _ = s.p.st.run(ctx, s.t.Smbcontrol, []string{"smbd", "reload-config"})
	// Samba memeriksa izin saat koneksi dibuka: koneksi ke folder yang
	// izinnya berubah atau dicabut ditutup, agar perubahan langsung berlaku.
	before, after := smbSections(old), smbSections(data)
	for name, sec := range before {
		if name != "global" && after[name] != sec {
			_, _ = s.p.st.run(ctx, s.t.Smbcontrol, []string{"smbd", "close-share"}, name)
		}
	}
	return "applied", nil
}

// smbSections memecah smb.conf menjadi bagian per nama ([nama] → isi).
func smbSections(conf []byte) map[string]string {
	out := map[string]string{}
	name := ""
	var cur strings.Builder
	flush := func() {
		if name != "" {
			out[name] = cur.String()
		}
	}
	for _, line := range strings.Split(string(conf), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			flush()
			name, cur = strings.Trim(t, "[]"), strings.Builder{}
			continue
		}
		cur.WriteString(line + "\n")
	}
	flush()
	return out
}

// DeleteOp mengembalikan handler shares.delete: menghapus dataset folder
// beserta isinya (dan snapshot-nya). Destruktif; sapo-api sudah meminta
// password dan nama diketik ulang, dan mencatatnya di log audit.
func (s *Shares) DeleteOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.ShareDeleteParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		if !shareNamePattern.MatchString(req.Name) || !poolNamePattern.MatchString(req.Pool) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama tidak valid"}
		}
		if !s.p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer s.p.write.Unlock()
		ctx, cancel := context.WithTimeout(ctx, s.p.t.CreateTimeout)
		defer cancel()
		ds := req.Pool + "/" + req.Name
		// Hanya dataset yang dibuat Sapo sebagai shared folder bernama sama.
		props, err := s.zfsProps(ctx, ds, "type", sharePropName)
		if errors.Is(err, errNoDataset) || (err == nil && (props["type"] != "filesystem" || props[sharePropName] != req.Name)) {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "shared folder " + ds + " tidak ditemukan"}
		}
		if err != nil {
			return nil, err
		}
		// Dataset anak (misalnya volume atau sistem berkas yang dipasang di
		// tempat lain) tidak ikut terhapus diam-diam.
		res, err := s.p.st.run(ctx, s.t.Zfs, []string{"list", "-H", "-r", "-t", "filesystem,volume", "-o", "name"}, ds)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != ds {
			return nil, &OpError{Code: proto.ErrConflict, Message: "shared folder " + ds + " berisi dataset lain"}
		}
		path := filepath.Join(s.t.MountBase, req.Pool, req.Name)
		// Folder yang masih dipasang di container aplikasi tidak dihapus.
		if s.apps != nil {
			if id := s.apps.usingShare(path); id != "" {
				return nil, &OpError{Code: proto.ErrConflict, Message: "shared folder " + ds + " masih dipakai aplikasi " + id + "; lepas dulu di App Center"}
			}
		}
		s.stopACLJob(ds)
		// Koneksi SMB yang masih membuka folder ditutup dulu, lalu folder
		// dilepas. Bila masih dipakai, tidak ada yang dihapus (zfs destroy -r
		// menghapus snapshot sebelum melepas folder).
		_, _ = s.p.st.run(ctx, s.t.Smbcontrol, []string{"smbd", "close-share"}, req.Name)
		// Ekspor NFS dicabut sebelum folder dilepas.
		if err := s.unexport(ctx, func(p string) bool { return p == path }); err != nil {
			return nil, &OpError{Code: "share_delete_failed", Message: clipTo(err.Error(), maxErrorText)}
		}
		res, err = s.p.st.run(ctx, s.t.Zfs, []string{"unmount"}, ds)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			msg := clipTo(strings.TrimSpace(string(res.Stderr)), maxErrorText)
			code := "share_delete_failed"
			if strings.Contains(strings.ToLower(msg), "busy") {
				code = proto.ErrPoolBusy
			}
			return nil, &OpError{Code: code, Message: msg}
		}
		s.p.logInfo("menghapus shared folder", "dataset", ds)
		res, err = s.p.st.run(ctx, s.t.Zfs, []string{"destroy", "-r"}, ds)
		if err == nil && res.ExitCode != 0 {
			err = errors.New(clipTo(strings.TrimSpace(string(res.Stderr)), maxErrorText))
		}
		if err != nil {
			// Gagal: folder dipasang lagi agar tetap bisa dipakai.
			_, _ = s.p.st.run(ctx, s.t.Zfs, []string{"mount"}, ds)
			return nil, &OpError{Code: "share_delete_failed", Message: err.Error()}
		}
		s.p.logInfo("shared folder dihapus", "dataset", ds)
		return map[string]bool{"ok": true}, nil
	}
}

// UsageOp mengembalikan handler shares.usage: ruang terpakai dan tersedia
// setiap dataset folder (baca saja).
func (s *Shares) UsageOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, s.p.st.t.Timeout)
		defer cancel()
		res, err := s.p.st.run(ctx, s.t.Zfs, []string{"list", "-H", "-p", "-t", "filesystem", "-o", "name,used,avail"})
		if errors.Is(err, errToolMissing) {
			return map[string]proto.ShareUsage{}, nil
		}
		if err != nil {
			return nil, err
		}
		out := map[string]proto.ShareUsage{}
		sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
		for sc.Scan() {
			f := strings.Split(sc.Text(), "\t")
			// Hanya <pool>/<folder> (satu tingkat di bawah pool).
			if len(f) != 3 || strings.Count(f[0], "/") != 1 {
				continue
			}
			used, _ := strconv.ParseUint(f[1], 10, 64)
			avail, _ := strconv.ParseUint(f[2], 10, 64)
			out[f[0]] = proto.ShareUsage{UsedBytes: used, AvailableBytes: avail}
		}
		s.jobsMu.Lock()
		for ds, j := range s.jobs {
			u := out[ds]
			u.ACLPending, u.ACLError = !j.done, j.err
			out[ds] = u
		}
		s.jobsMu.Unlock()
		return out, nil
	}
}
