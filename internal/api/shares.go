package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/auth"
	"github.com/mehuli-id/sapo/internal/proto"
)

// --- Shared folder -----------------------------------------------------------
//
// Definisi folder (nama, pool, keterangan, izin) disimpan sapo-api di
// <data_dir>/shares.json. Setiap perubahan pengguna, grup, atau folder
// diteruskan ke sapod sebagai keadaan lengkap (accounts.sync lalu
// shares.apply), sehingga sistem selalu bisa disamakan ulang, misalnya saat
// sapo-api menyala. Menghapus folder (beserta isinya) adalah operasi
// tersendiri yang destruktif.

// Share adalah satu shared folder.
type Share struct {
	Name        string `json:"name"`
	Pool        string `json:"pool"`
	Description string `json:"description,omitempty"`
	// Permissions: "user:nama" / "group:nama" → "read" atau "write".
	Permissions map[string]string `json:"permissions"`
	// NFS: folder juga diekspor lewat NFS (nil = tidak).
	NFS       *ShareNFS `json:"nfs,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ShareNFS: klien di jaringan Clients boleh memasang folder lewat NFS, dan
// semua aksesnya berlaku sebagai pengguna User (izin pengguna itu di
// folder, termasuk lewat grup).
type ShareNFS struct {
	Clients []string `json:"clients"`
	User    string   `json:"user"`
}

func (n *ShareNFS) clone() *ShareNFS {
	if n == nil {
		return nil
	}
	return &ShareNFS{Clients: slices.Clone(n.Clients), User: n.User}
}

type shareStore struct {
	path   string
	mu     sync.Mutex
	shares []Share
}

func openShareStore(dir string) (*shareStore, error) {
	st := &shareStore{path: filepath.Join(dir, "shares.json")}
	data, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &st.shares); err != nil {
		return nil, fmt.Errorf("file shared folder rusak (%s): %w", st.path, err)
	}
	return st, nil
}

func (st *shareStore) list() []Share {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Share, len(st.shares))
	for i, sh := range st.shares {
		sh.Permissions = cloneMap(sh.Permissions)
		sh.NFS = sh.NFS.clone()
		out[i] = sh
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (st *shareStore) get(name string) (Share, bool) {
	for _, sh := range st.list() {
		if sh.Name == name {
			return sh, true
		}
	}
	return Share{}, false
}

var errShareExists = errors.New("nama shared folder sudah dipakai")

// mutate menerapkan fn pada salinan daftar lalu menyimpannya (atomik).
func (st *shareStore) mutate(fn func([]Share) ([]Share, error)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	cur := make([]Share, len(st.shares))
	for i, sh := range st.shares {
		sh.Permissions = cloneMap(sh.Permissions)
		sh.NFS = sh.NFS.clone()
		cur[i] = sh
	}
	next, err := fn(cur)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(st.path, data); err != nil {
		return err
	}
	st.shares = next
	return nil
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func writeFileAtomic(path string, data []byte) error {
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

// --- Sinkronisasi ke sapod ---------------------------------------------------

// syncSystem mengirim keadaan lengkap pengguna, grup, dan folder ke sapod.
// Hanya satu sinkronisasi berjalan pada satu waktu.
func (s *Server) syncSystem(ctx context.Context) (res proto.SharesApplyResult, err error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	defer func() {
		s.syncStateMu.Lock()
		// Jam sistem (bukan s.now): fungsi ini juga berjalan di goroutine
		// latar belakang.
		s.syncLastErr, s.syncLastAt = "", time.Now().UTC()
		if err != nil {
			s.syncLastErr = clipText(err.Error(), 2000)
		}
		s.syncStateMu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()

	users, groups := s.store.Users(), s.store.Groups()
	byName := map[string]auth.User{}
	for _, u := range users {
		byName[u.Username] = u
	}
	var acc proto.AccountsSyncParams
	acc.Users, acc.Groups = []proto.AccountUser{}, []string{}
	exists := map[string]bool{}
	for _, g := range groups {
		acc.Groups = append(acc.Groups, g.Name)
		exists["group:"+g.Name] = true
	}
	for _, u := range users {
		acc.Users = append(acc.Users, proto.AccountUser{Name: u.Username, Groups: u.Groups, Disabled: u.Disabled})
		exists["user:"+u.Username] = true
	}
	var accRes proto.AccountsSyncResult
	if err := s.agent.Call(ctx, proto.OpAccountsSync, acc, &accRes); err != nil {
		// Akun tidak bisa dipastikan: semua izin folder dicabut (gagal-
		// tertutup), agar pencabutan izin di panel tetap berlaku.
		var pe *proto.Error
		if errors.As(err, &pe) {
			closed := proto.SharesApplyParams{Shares: []proto.ShareSpec{}}
			for _, sh := range s.shares.list() {
				closed.Shares = append(closed.Shares, proto.ShareSpec{Name: sh.Name, Pool: sh.Pool, Description: sh.Description, Read: []string{}, Write: []string{}})
			}
			_ = s.agent.Call(ctx, proto.OpSharesApply, closed, nil)
		}
		return res, err
	}
	// Akun yang gagal dibuat tidak mendapat izin folder; sisanya diterapkan.
	failed := false
	for _, f := range accRes.Failures {
		name, _, _ := strings.Cut(f, ": ")
		delete(exists, name)
		failed = true
	}
	if failed && s.log != nil {
		s.log.Warn("sebagian akun sistem gagal disinkronkan", "gagal", accRes.Failures)
	}
	failures := strings.Join(accRes.Failures, "; ")
	// Nama yang bentrok dengan akun sistem lain tidak mendapat izin folder.
	for _, c := range accRes.Conflicts {
		delete(exists, c)
	}
	if len(accRes.Conflicts) > 0 && s.log != nil {
		s.log.Warn("nama pengguna atau grup bentrok dengan akun sistem", "bentrok", accRes.Conflicts)
	}

	params := proto.SharesApplyParams{Shares: []proto.ShareSpec{}}
	for _, sh := range s.shares.list() {
		spec := proto.ShareSpec{Name: sh.Name, Pool: sh.Pool, Description: sh.Description, Read: []string{}, Write: []string{}}
		for p, level := range sh.Permissions {
			if !exists[p] {
				continue
			}
			if level == "write" {
				spec.Write = append(spec.Write, p)
			} else {
				spec.Read = append(spec.Read, p)
			}
		}
		slices.Sort(spec.Read)
		slices.Sort(spec.Write)
		if sh.NFS != nil && exists["user:"+sh.NFS.User] {
			// Ekspor hanya untuk pengguna aktif yang punya akses ke folder.
			if u, ok := byName[sh.NFS.User]; ok && !u.Disabled {
				switch nfsAccess(sh, u, exists) {
				case "write":
					spec.NFS = &proto.NFSExport{Clients: slices.Clone(sh.NFS.Clients), User: u.Username}
				case "read":
					spec.NFS = &proto.NFSExport{Clients: slices.Clone(sh.NFS.Clients), User: u.Username, ReadOnly: true}
				}
			}
		}
		params.Shares = append(params.Shares, spec)
	}
	err = s.agent.Call(ctx, proto.OpSharesApply, params, &res)
	if err == nil {
		states := map[string]proto.ShareState{}
		for _, st := range res.Shares {
			states[st.Name] = st
		}
		s.syncStateMu.Lock()
		s.syncShares = states
		s.syncStateMu.Unlock()
	}
	if err == nil && failed {
		err = errors.New("sebagian akun sistem gagal disinkronkan: " + failures)
	}
	return res, err
}

// nfsAccess: izin efektif pengguna u di folder ("write", "read", atau "")
// menurut aturan ACL POSIX di sistem berkas: izin langsung pengguna berlaku
// bila ada; bila tidak, izin terluas dari grup yang diikutinya. Hanya izin
// yang ikut diterapkan (ada di exists) yang dihitung.
func nfsAccess(sh Share, u auth.User, exists map[string]bool) string {
	if p := "user:" + u.Username; exists[p] && sh.Permissions[p] != "" {
		return sh.Permissions[p]
	}
	best := ""
	for _, g := range u.Groups {
		p := "group:" + g
		if !exists[p] {
			continue
		}
		switch sh.Permissions[p] {
		case "write":
			best = "write"
		case "read":
			if best == "" {
				best = "read"
			}
		}
	}
	return best
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// syncAfterChange menjalankan sinkronisasi setelah perubahan di panel.
// Kegagalan dicatat dan dilaporkan ke panel sebagai peringatan; perubahan
// di panel tetap tersimpan dan akan diterapkan pada sinkronisasi berikutnya.
func (s *Server) syncAfterChange(r *http.Request) string {
	if _, err := s.syncSystem(r.Context()); err != nil {
		if s.log != nil {
			s.log.Warn("sinkronisasi akun dan shared folder gagal; dicoba ulang di latar belakang", "err", err)
		}
		s.scheduleResync()
		return "system_sync_failed"
	}
	return ""
}

// handleSyncStatus: status sinkronisasi akun dan shared folder terakhir
// (untuk peringatan di panel dan diagnosis).
func (s *Server) handleSyncStatus(w http.ResponseWriter, _ *http.Request) {
	s.syncStateMu.Lock()
	defer s.syncStateMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": s.syncLastErr == "", "error": s.syncLastErr, "retrying": s.resyncRunning.Load(), "at": s.syncLastAt})
}

// SyncLoop menyamakan sistem saat sapo-api menyala (sapod mungkin belum
// siap, jadi dicoba ulang).
func (s *Server) SyncLoop(ctx context.Context) {
	for attempt := 0; attempt < 20; attempt++ {
		_, err := s.syncSystem(ctx)
		if err == nil {
			return
		}
		if attempt == 19 {
			if s.log != nil {
				s.log.Warn("sinkronisasi awal akun dan shared folder gagal; dicoba ulang di latar belakang", "err", err)
			}
			s.scheduleResync()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// setSMBPassword menyamakan password Samba (password hanya tersedia saat
// login, dibuat, atau diganti). Samba yang belum terpasang diabaikan.
//
// hash adalah hash password panel yang cocok dengan password ini (kosong =
// tidak diperiksa). Permintaan per pengguna berurutan, dan dilewati bila
// password panel sudah berganti atau akunnya nonaktif, sehingga login lama
// yang tertunda tidak bisa memasang kembali password lama.
func (s *Server) setSMBPassword(ctx context.Context, username, password, hash string) {
	mu := s.smbLock(username)
	mu.Lock()
	defer mu.Unlock()
	u, ok := s.store.Get(username)
	if !ok || u.Disabled || (hash != "" && u.PasswordHash != hash) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := s.agent.Call(ctx, proto.OpSMBPassword, proto.SMBPasswordParams{Name: username, Password: password}, nil)
	var pe *proto.Error
	if err != nil && !(errors.As(err, &pe) && pe.Code == proto.ErrUnavailable) && s.log != nil {
		s.log.Warn("password Samba tidak bisa disamakan", "user", username, "err", err)
	}
}

func (s *Server) smbLock(username string) *sync.Mutex {
	s.smbMu.Lock()
	defer s.smbMu.Unlock()
	if s.smbLocks == nil {
		s.smbLocks = map[string]*sync.Mutex{}
	}
	mu, ok := s.smbLocks[username]
	if !ok {
		mu = &sync.Mutex{}
		s.smbLocks[username] = mu
	}
	return mu
}

// scheduleResync mencoba ulang sinkronisasi di latar belakang sampai
// berhasil (misalnya sapod sedang sibuk dengan operasi pool).
func (s *Server) scheduleResync() {
	if !s.resyncRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.resyncRunning.Store(false)
		for attempt := 0; attempt < 240; attempt++ {
			time.Sleep(s.resyncDelay)
			if _, err := s.syncSystem(context.Background()); err == nil {
				if s.log != nil {
					s.log.Info("sinkronisasi akun dan shared folder berhasil setelah dicoba ulang")
				}
				return
			}
		}
	}()
}

// --- Handler -----------------------------------------------------------------

var shareNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

var reservedShareNames = []string{"global", "homes", "printers", "print", "ipc"}

type shareView struct {
	Share
	Path           string `json:"path,omitempty"`
	Error          string `json:"error,omitempty"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	ACLPending     bool   `json:"acl_pending,omitempty"`
	ACLError       string `json:"acl_error,omitempty"`
	// NFSAccess: izin efektif pengguna NFS di folder ("write", "read", atau
	// kosong = tidak diekspor); NFSError: galat ekspor terakhir dari sapod.
	NFSAccess string `json:"nfs_access,omitempty"`
	NFSError  string `json:"nfs_error,omitempty"`
}

// shareStatus melengkapi tampilan folder dengan hasil sinkronisasi terakhir
// dan izin efektif pengguna NFS.
func (s *Server) shareStatus(v *shareView) {
	s.syncStateMu.Lock()
	st, ok := s.syncShares[v.Name]
	s.syncStateMu.Unlock()
	if ok {
		v.Path, v.Error, v.NFSError = st.Path, st.Error, st.NFSError
	}
	if v.NFS == nil {
		return
	}
	if u, ok := s.store.Get(v.NFS.User); ok && !u.Disabled {
		all := map[string]bool{}
		for p := range v.Permissions {
			all[p] = true
		}
		v.NFSAccess = nfsAccess(v.Share, u, all)
	}
}

// nfsNetworks: klien NFS hanya di jaringan lokal (sama dengan sapod).
var nfsNetworks = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// normalizeNFS memeriksa ekspor NFS dari panel: pengguna harus ada, klien
// berupa alamat IP atau jaringan lokal (nama host tidak diterima karena bisa
// dipalsukan lewat DNS). Alamat tunggal menjadi /32 atau /128, jaringan
// disamakan ke bentuk kanonis, dan duplikat dibuang. Alamat publik dan
// jaringan yang lebih luas dari jaringan lokal ditolak.
func (s *Server) normalizeNFS(in *ShareNFS) (*ShareNFS, bool) {
	if in == nil {
		return nil, true
	}
	if _, ok := s.store.Get(in.User); !ok || len(in.Clients) == 0 || len(in.Clients) > proto.MaxNFSClients {
		return nil, false
	}
	out := &ShareNFS{User: in.User, Clients: []string{}}
	for _, c := range in.Clients {
		c = strings.TrimSpace(c)
		var p netip.Prefix
		if a, err := netip.ParseAddr(c); err == nil {
			p = netip.PrefixFrom(a, a.BitLen())
		} else if p, err = netip.ParsePrefix(c); err != nil {
			return nil, false
		}
		p = p.Masked()
		a := p.Addr()
		if a.Zone() != "" || a.Is4In6() || !slices.ContainsFunc(nfsNetworks, func(n netip.Prefix) bool { return n.Bits() <= p.Bits() && n.Contains(a) }) {
			return nil, false
		}
		if !slices.Contains(out.Clients, p.String()) {
			out.Clients = append(out.Clients, p.String())
		}
	}
	return out, true
}

func nfsText(n *ShareNFS) string {
	if n == nil {
		return "off"
	}
	return auditText(n.User + "@" + strings.Join(n.Clients, ","))
}

func (s *Server) handleShares(w http.ResponseWriter, r *http.Request) {
	usage := map[string]proto.ShareUsage{}
	_ = s.agent.Call(r.Context(), proto.OpSharesUsage, nil, &usage)
	out := []shareView{}
	for _, sh := range s.shares.list() {
		u := usage[sh.Pool+"/"+sh.Name]
		v := shareView{Share: sh, UsedBytes: u.UsedBytes, AvailableBytes: u.AvailableBytes, ACLPending: u.ACLPending, ACLError: u.ACLError}
		s.shareStatus(&v)
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// validPermissions: kunci harus pengguna atau grup yang ada; nilai read/write.
func (s *Server) validPermissions(perms map[string]string) bool {
	if len(perms) > 1000 {
		return false
	}
	for p, level := range perms {
		if level != "read" && level != "write" {
			return false
		}
		kind, name, ok := strings.Cut(p, ":")
		switch {
		case !ok:
			return false
		case kind == "user":
			if _, exists := s.store.Get(name); !exists {
				return false
			}
		case kind == "group":
			if !s.groupExists(name) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (s *Server) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string            `json:"name"`
		Pool          string            `json:"pool"`
		Description   string            `json:"description"`
		Permissions   map[string]string `json:"permissions"`
		NFS           *ShareNFS         `json:"nfs"`
		AdminPassword string            `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(strings.ToLower(req.Name))
	nfs, nfsOK := s.normalizeNFS(req.NFS)
	switch {
	case !shareNameRe.MatchString(req.Name) || slices.Contains(reservedShareNames, req.Name):
		writeErrorDetail(w, http.StatusBadRequest, "share_invalid", "nama shared folder tidak sesuai aturan", map[string]any{"field": "name"})
		return
	case !shareNameRe.MatchString(req.Pool):
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "pool tidak valid", map[string]any{"field": "pool"})
		return
	case !validShareDescription(req.Description):
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "keterangan tidak valid", map[string]any{"field": "description"})
		return
	case !s.validPermissions(req.Permissions):
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "izin tidak valid", map[string]any{"field": "permissions"})
		return
	case !nfsOK:
		writeErrorDetail(w, http.StatusBadRequest, "nfs_invalid", "ekspor NFS tidak valid", map[string]any{"field": "nfs"})
		return
	}
	// Memberi akses ke data: butuh password admin.
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	sh := Share{Name: req.Name, Pool: req.Pool, Description: req.Description, Permissions: cloneMap(req.Permissions), NFS: nfs, CreatedAt: s.now().UTC()}
	err := s.shares.mutate(func(cur []Share) ([]Share, error) {
		if slices.ContainsFunc(cur, func(x Share) bool { return x.Name == sh.Name }) {
			return nil, errShareExists
		}
		return append(cur, sh), nil
	})
	if errors.Is(err, errShareExists) {
		writeErrorDetail(w, http.StatusConflict, "share_taken", "nama shared folder sudah dipakai", map[string]any{"field": "name"})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan shared folder")
		return
	}
	_, syncErr := s.syncSystem(r.Context())
	view := shareView{Share: sh}
	s.shareStatus(&view)
	if syncErr != nil {
		view.Error = "system_sync_failed"
	}
	s.record(r, admin.Username, "shares.create", view.Error == "", map[string]string{"share": sh.Name, "pool": sh.Pool, "permissions": permText(sh.Permissions), "nfs": nfsText(sh.NFS)})
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleShareUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Description *string            `json:"description"`
		Permissions *map[string]string `json:"permissions"`
		// NFS: tidak ada = tidak berubah, null = ekspor NFS dimatikan.
		NFS           json.RawMessage `json:"nfs"`
		AdminPassword string          `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	nfsChanged := len(req.NFS) > 0
	var nfs *ShareNFS
	nfsOK := true
	if nfsChanged && string(req.NFS) != "null" {
		var in ShareNFS
		if err := json.Unmarshal(req.NFS, &in); err != nil {
			nfsOK = false
		} else {
			nfs, nfsOK = s.normalizeNFS(&in)
		}
	}
	name := r.PathValue("name")
	if _, ok := s.shares.get(name); !ok {
		writeError(w, http.StatusNotFound, "not_found", "shared folder tidak ditemukan")
		return
	}
	if (req.Description != nil && !validShareDescription(*req.Description)) || (req.Permissions != nil && !s.validPermissions(*req.Permissions)) {
		writeError(w, http.StatusBadRequest, "bad_request", "isi permintaan tidak valid")
		return
	}
	if !nfsOK {
		writeErrorDetail(w, http.StatusBadRequest, "nfs_invalid", "ekspor NFS tidak valid", map[string]any{"field": "nfs"})
		return
	}
	sess, _, _ := s.currentSession(r)
	actor := sess.Username
	// Memberi akses (izin atau ekspor NFS): butuh password admin.
	if req.Permissions != nil || nfsChanged {
		admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
		if !ok {
			return
		}
		actor = admin.Username
	}
	err := s.shares.mutate(func(cur []Share) ([]Share, error) {
		for i := range cur {
			if cur[i].Name == name {
				if req.Description != nil {
					cur[i].Description = *req.Description
				}
				if req.Permissions != nil {
					cur[i].Permissions = cloneMap(*req.Permissions)
				}
				if nfsChanged {
					cur[i].NFS = nfs.clone()
				}
			}
		}
		return cur, nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan shared folder")
		return
	}
	warning := s.syncAfterChange(r)
	detail := map[string]string{"share": name}
	if req.Permissions != nil {
		detail["permissions"] = permText(*req.Permissions)
	}
	if nfsChanged {
		detail["nfs"] = nfsText(nfs)
	}
	s.record(r, actor, "shares.update", warning == "", detail)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
}

func (s *Server) handleShareDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ConfirmName   string `json:"confirm_name"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	sh, ok := s.shares.get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "shared folder tidak ditemukan")
		return
	}
	if req.ConfirmName != name {
		writeErrorDetail(w, http.StatusBadRequest, "confirm_mismatch", "nama yang diketik ulang tidak sama", map[string]any{"field": "confirm_name"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	// Data dihapus dulu (sapod menutup koneksi SMB ke folder itu); baru
	// setelah berhasil, folder keluar dari daftar dan dari konfigurasi Samba.
	// Bila gagal (misalnya folder sedang dipakai), folder tetap seperti semula.
	s.destructive(w, r, admin, "shares.delete", map[string]string{"share": name, "pool": sh.Pool}, proto.OpSharesDelete,
		proto.ShareDeleteParams{Name: name, Pool: sh.Pool}, func() {
			if err := s.shares.mutate(func(cur []Share) ([]Share, error) {
				return slices.DeleteFunc(cur, func(x Share) bool { return x.Name == name }), nil
			}); err != nil && s.log != nil {
				s.log.Error("shared folder terhapus tetapi daftar gagal disimpan", "share", name, "err", err)
			}
			s.syncAfterChange(r)
		})
}

func validShareDescription(d string) bool {
	return validDescription(d) && utf8.RuneCountInString(d) <= 64 && !strings.ContainsAny(d, "[]\\%")
}

func permText(p map[string]string) string {
	keys := make([]string, 0, len(p))
	for k, v := range p {
		keys = append(keys, k+"="+v)
	}
	slices.Sort(keys)
	return auditText(strings.Join(keys, ","))
}

// purgePrincipal membuang izin pengguna atau grup yang dihapus dari semua
// folder.
func (s *Server) purgePrincipal(p string) {
	_ = s.shares.mutate(func(cur []Share) ([]Share, error) {
		for i := range cur {
			delete(cur[i].Permissions, p)
			// Ekspor NFS pengguna yang dihapus ikut dimatikan.
			if cur[i].NFS != nil && "user:"+cur[i].NFS.User == p {
				cur[i].NFS = nil
			}
		}
		return cur, nil
	})
}
