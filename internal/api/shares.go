package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	CreatedAt   time.Time         `json:"created_at"`
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
func (s *Server) syncSystem(ctx context.Context) (proto.SharesApplyResult, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()

	users, groups := s.store.Users(), s.store.Groups()
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
		return proto.SharesApplyResult{}, err
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
		params.Shares = append(params.Shares, spec)
	}
	var res proto.SharesApplyResult
	err := s.agent.Call(ctx, proto.OpSharesApply, params, &res)
	if err == nil && failed {
		err = errors.New("sebagian akun sistem gagal disinkronkan")
	}
	return res, err
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

func (s *Server) currentHash(username string) string {
	u, _ := s.store.Get(username)
	return u.PasswordHash
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
}

func (s *Server) handleShares(w http.ResponseWriter, r *http.Request) {
	usage := map[string]proto.ShareUsage{}
	_ = s.agent.Call(r.Context(), proto.OpSharesUsage, nil, &usage)
	out := []shareView{}
	for _, sh := range s.shares.list() {
		u := usage[sh.Pool+"/"+sh.Name]
		out = append(out, shareView{Share: sh, UsedBytes: u.UsedBytes, AvailableBytes: u.AvailableBytes, ACLPending: u.ACLPending, ACLError: u.ACLError})
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
		AdminPassword string            `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(strings.ToLower(req.Name))
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
	}
	// Memberi akses ke data: butuh password admin.
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	sh := Share{Name: req.Name, Pool: req.Pool, Description: req.Description, Permissions: cloneMap(req.Permissions), CreatedAt: s.now().UTC()}
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
	res, syncErr := s.syncSystem(r.Context())
	view := shareView{Share: sh}
	for _, st := range res.Shares {
		if st.Name == sh.Name {
			view.Path, view.Error = st.Path, st.Error
		}
	}
	if syncErr != nil {
		view.Error = "system_sync_failed"
	}
	s.record(r, admin.Username, "shares.create", view.Error == "", map[string]string{"share": sh.Name, "pool": sh.Pool, "permissions": permText(sh.Permissions)})
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleShareUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Description   *string            `json:"description"`
		Permissions   *map[string]string `json:"permissions"`
		AdminPassword string             `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
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
	sess, _, _ := s.currentSession(r)
	actor := sess.Username
	if req.Permissions != nil {
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
		}
		return cur, nil
	})
}
