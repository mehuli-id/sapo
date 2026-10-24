package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// AccountTools adalah program dan kebijakan akun sistem untuk berbagi file
// (agent.accounts di sapo.yaml).
//
// Setiap pengguna Sapo punya akun Linux dengan nama yang sama (tanpa shell,
// tanpa home) agar bisa memiliki file dan masuk lewat SMB. sapod hanya
// menyentuh akun dan grup yang ia buat sendiri: tercatat di StateFile dengan
// UID/GID yang sama, di dalam rentang [IDMin, IDMax]. ID tidak pernah dipakai
// ulang, sehingga akun baru tidak mewarisi file dan ACL akun yang dihapus.
type AccountTools struct {
	Useradd, Userdel, Groupadd, Groupdel, Gpasswd string
	Smbpasswd, Pkill                              string
	PasswdFile, GroupFile                         string
	StateFile                                     string
	IDMin, IDMax                                  int
	PrimaryGroup                                  string
	Shell                                         string
	Timeout                                       time.Duration
	Log                                           *slog.Logger
}

// Accounts melayani accounts.sync dan smb.set_password. Hanya satu
// perubahan akun yang berjalan pada satu waktu.
type Accounts struct {
	t  AccountTools
	mu sync.Mutex
}

// NewAccounts menyiapkan operasi akun.
func NewAccounts(t AccountTools) *Accounts { return &Accounts{t: t} }

// accountNamePattern: nama akun dan grup yang diterima (sama dengan aturan
// Sapo; tidak diawali tanda hubung atau angka).
var accountNamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)

// accountState adalah catatan sapod tentang akun dan grup buatannya.
type accountState struct {
	// NextID: ID berikutnya; hanya naik, tidak pernah dipakai ulang.
	NextID  int                     `json:"next_id"`
	Primary int                     `json:"primary_gid,omitempty"`
	Users   map[string]stateAccount `json:"users"`
	Groups  map[string]int          `json:"groups"`
}

type stateAccount struct {
	UID int `json:"uid"`
	// SMBDisabled: status terakhir yang berhasil diterapkan ke Samba (nil =
	// belum pernah).
	SMBDisabled *bool `json:"smb_disabled,omitempty"`
}

func (a *Accounts) loadState() (accountState, error) {
	st := accountState{NextID: a.t.IDMin, Users: map[string]stateAccount{}, Groups: map[string]int{}}
	data, err := os.ReadFile(a.t.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("catatan akun rusak (%s): %w", a.t.StateFile, err)
	}
	if st.Users == nil {
		st.Users = map[string]stateAccount{}
	}
	if st.Groups == nil {
		st.Groups = map[string]int{}
	}
	return st, nil
}

// saveState menulis catatan secara atomik (0600, direktori milik root).
func (a *Accounts) saveState(st accountState) error {
	dir := filepath.Dir(a.t.StateFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*")
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
	return os.Rename(tmp.Name(), a.t.StateFile)
}

// allocID mengambil ID berikutnya yang belum dipakai akun atau grup mana pun,
// lalu langsung mencatatnya (agar ID tidak dipakai ulang walau langkah
// berikutnya gagal).
func (a *Accounts) allocID(st *accountState, users map[string]passwdEntry, groups map[string]groupEntry) (int, error) {
	used := map[int]bool{}
	for _, u := range users {
		used[u.UID] = true
	}
	for _, g := range groups {
		used[g.GID] = true
	}
	id := max(st.NextID, a.t.IDMin)
	for ; id <= a.t.IDMax && used[id]; id++ {
	}
	if id > a.t.IDMax {
		return 0, &OpError{Code: proto.ErrConflict, Message: "rentang ID akun Sapo sudah habis"}
	}
	st.NextID = id + 1
	return id, a.saveState(*st)
}

type passwdEntry struct {
	Name     string
	UID, GID int
}

type groupEntry struct {
	Name    string
	GID     int
	Members []string
}

// readPasswd membaca /etc/passwd (hanya nama, UID, GID).
func readPasswd(path string) (map[string]passwdEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]passwdEntry{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) < 4 {
			continue
		}
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		out[f[0]] = passwdEntry{Name: f[0], UID: uid, GID: gid}
	}
	return out, sc.Err()
}

// readGroup membaca /etc/group.
func readGroup(path string) (map[string]groupEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]groupEntry{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) < 4 {
			continue
		}
		gid, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		var members []string
		if f[3] != "" {
			members = strings.Split(f[3], ",")
		}
		out[f[0]] = groupEntry{Name: f[0], GID: gid, Members: members}
	}
	return out, sc.Err()
}

func (a *Accounts) inRange(id int) bool { return id >= a.t.IDMin && id <= a.t.IDMax }

func (a *Accounts) run(ctx context.Context, prog string, flags []string, operands ...string) error {
	res, err := runTool(ctx, a.t.Timeout, prog, flags, operands...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s keluar dengan kode %d: %s", filepath.Base(prog), res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// SyncOp mengembalikan handler accounts.sync: menyamakan akun dan grup
// sistem buatan Sapo dengan daftar pengguna dan grup di panel. Operasi ini
// deklaratif dan aman diulang. Kegagalan satu akun tidak menghentikan yang
// lain; semuanya dilaporkan di Failures.
func (a *Accounts) SyncOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.AccountsSyncParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		if err := a.checkSync(req); err != nil {
			return nil, err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.sync(ctx, req)
	}
}

func (a *Accounts) checkSync(req proto.AccountsSyncParams) error {
	invalid := func(msg string) error { return &OpError{Code: proto.ErrInvalidParams, Message: msg} }
	if len(req.Users) > 10000 || len(req.Groups) > 10000 {
		return invalid("terlalu banyak akun")
	}
	groups := map[string]bool{}
	for _, g := range req.Groups {
		if !accountNamePattern.MatchString(g) || g == a.t.PrimaryGroup {
			return invalid("nama grup tidak valid: " + clip(g))
		}
		groups[g] = true
	}
	seen := map[string]bool{}
	for _, u := range req.Users {
		if !accountNamePattern.MatchString(u.Name) || seen[u.Name] || groups[u.Name] {
			return invalid("nama pengguna tidak valid: " + clip(u.Name))
		}
		seen[u.Name] = true
		for _, g := range u.Groups {
			if !groups[g] {
				return invalid("grup " + clip(g) + " milik " + u.Name + " tidak ada di daftar")
			}
		}
	}
	return nil
}

func (a *Accounts) sync(ctx context.Context, req proto.AccountsSyncParams) (proto.AccountsSyncResult, error) {
	out := proto.AccountsSyncResult{Conflicts: []string{}, Failures: []string{}}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	fail := func(what string, err error) {
		out.Failures = append(out.Failures, what+": "+clipTo(err.Error(), maxErrorText))
	}

	st, err := a.loadState()
	if err != nil {
		return out, err
	}
	users, err := readPasswd(a.t.PasswdFile)
	if err != nil {
		return out, err
	}
	groups, err := readGroup(a.t.GroupFile)
	if err != nil {
		return out, err
	}

	// Grup utama penanda akun Sapo: dibuat sapod sendiri dan tercatat.
	pg, ok := groups[a.t.PrimaryGroup]
	switch {
	case !ok:
		id, err := a.allocID(&st, users, groups)
		if err != nil {
			return out, err
		}
		if err := a.run(ctx, a.t.Groupadd, []string{"--gid", strconv.Itoa(id)}, a.t.PrimaryGroup); err != nil {
			return out, err
		}
		st.Primary = id
		if err := a.saveState(st); err != nil {
			return out, err
		}
		pg = groupEntry{Name: a.t.PrimaryGroup, GID: id}
	case pg.GID != st.Primary || !a.inRange(pg.GID):
		// Grup bernama sama yang tidak dibuat sapod: tidak diambil alih.
		return out, &OpError{Code: proto.ErrConflict, Message: fmt.Sprintf("grup %s (GID %d) bukan buatan Sapo", a.t.PrimaryGroup, pg.GID)}
	}
	managedUser := func(name string) bool {
		s, rec := st.Users[name]
		e, exists := users[name]
		return rec && exists && e.UID == s.UID && e.GID == pg.GID && a.inRange(e.UID)
	}
	managedGroup := func(name string) bool {
		gid, rec := st.Groups[name]
		e, exists := groups[name]
		return rec && exists && e.GID == gid && a.inRange(gid)
	}

	// 1. Grup yang dibutuhkan.
	wantGroups := map[string]bool{}
	for _, g := range req.Groups {
		_, exists := groups[g]
		switch {
		case managedGroup(g):
			wantGroups[g] = true
		case exists:
			// Nama sudah dipakai grup lain: tidak pernah diambil alih.
			out.Conflicts = append(out.Conflicts, "group:"+g)
		default:
			id, err := a.allocID(&st, users, groups)
			if err != nil {
				fail("group:"+g, err)
				continue
			}
			if err := a.run(ctx, a.t.Groupadd, []string{"--gid", strconv.Itoa(id)}, g); err != nil {
				fail("group:"+g, err)
				continue
			}
			st.Groups[g] = id
			groups[g] = groupEntry{Name: g, GID: id}
			wantGroups[g] = true
			out.CreatedGroups = append(out.CreatedGroups, g)
		}
	}

	// 2. Akun yang dibutuhkan.
	wantUsers := map[string]bool{}
	for _, u := range req.Users {
		_, exists := users[u.Name]
		switch {
		case managedUser(u.Name):
			wantUsers[u.Name] = true
		case exists:
			out.Conflicts = append(out.Conflicts, "user:"+u.Name)
		default:
			id, err := a.allocID(&st, users, groups)
			if err != nil {
				fail("user:"+u.Name, err)
				continue
			}
			flags := []string{"--uid", strconv.Itoa(id), "--no-create-home", "--home-dir", "/nonexistent", "--shell", a.t.Shell, "--gid", strconv.Itoa(pg.GID), "--no-user-group"}
			if err := a.run(ctx, a.t.Useradd, flags, u.Name); err != nil {
				fail("user:"+u.Name, err)
				continue
			}
			st.Users[u.Name] = stateAccount{UID: id}
			users[u.Name] = passwdEntry{Name: u.Name, UID: id, GID: pg.GID}
			wantUsers[u.Name] = true
			out.CreatedUsers = append(out.CreatedUsers, u.Name)
		}
	}
	if err := a.saveState(st); err != nil {
		return out, err
	}

	// 3. Anggota setiap grup Sapo (daftar lengkap).
	members := map[string][]string{}
	for _, u := range req.Users {
		if !wantUsers[u.Name] {
			continue
		}
		for _, g := range u.Groups {
			if wantGroups[g] {
				members[g] = append(members[g], u.Name)
			}
		}
	}
	if groups, err = readGroup(a.t.GroupFile); err != nil {
		return out, err
	}
	for g := range wantGroups {
		want := members[g]
		slices.Sort(want)
		have := slices.Clone(groups[g].Members)
		slices.Sort(have)
		if !slices.Equal(want, have) {
			if err := a.run(ctx, a.t.Gpasswd, []string{"--members", strings.Join(want, ",")}, g); err != nil {
				fail("group:"+g, err)
			}
		}
	}

	// 4. Akun Sapo yang tidak ada lagi di panel: sesinya diputus, akunnya
	//    dihapus (file miliknya tetap ada; UID-nya tidak dipakai ulang).
	for name := range st.Users {
		if wantUsers[name] {
			continue
		}
		if managedUser(name) {
			a.endSessions(ctx, users[name].UID)
			a.smb(ctx, "-x", name)
			if err := a.run(ctx, a.t.Userdel, nil, name); err != nil {
				fail("user:"+name, err)
				continue
			}
			out.RemovedUsers = append(out.RemovedUsers, name)
		}
		delete(st.Users, name)
	}
	for name := range st.Groups {
		if wantGroups[name] {
			continue
		}
		if managedGroup(name) {
			if err := a.run(ctx, a.t.Groupdel, nil, name); err != nil {
				fail("group:"+name, err)
				continue
			}
			out.RemovedGroups = append(out.RemovedGroups, name)
		}
		delete(st.Groups, name)
	}

	// 5. Status Samba hanya diubah bila berbeda dari yang terakhir
	//    diterapkan. Menonaktifkan juga memutus sesi yang sedang berjalan.
	for _, u := range req.Users {
		if !wantUsers[u.Name] {
			continue
		}
		s := st.Users[u.Name]
		if s.SMBDisabled != nil && *s.SMBDisabled == u.Disabled {
			continue
		}
		if u.Disabled {
			// Akun yang belum punya password Samba gagal di -d, tetapi memang
			// belum bisa masuk; status nonaktif diterapkan lagi saat password
			// Samba dibuat (SMBPasswordOp).
			_ = a.smb(ctx, "-d", u.Name)
			a.endSessions(ctx, s.UID)
		} else if err := a.smb(ctx, "-e", u.Name); err != nil && s.SMBDisabled != nil && *s.SMBDisabled {
			// Akun yang belum punya password Samba gagal di sini (wajar).
			fail("user:"+u.Name, err)
			continue
		}
		disabled := u.Disabled
		s.SMBDisabled = &disabled
		st.Users[u.Name] = s
	}
	if err := a.saveState(st); err != nil {
		return out, err
	}
	if len(out.CreatedUsers)+len(out.RemovedUsers)+len(out.CreatedGroups)+len(out.RemovedGroups)+len(out.Conflicts)+len(out.Failures) > 0 && a.t.Log != nil {
		a.t.Log.Info("akun sistem disinkronkan", "akun_baru", out.CreatedUsers, "akun_dihapus", out.RemovedUsers,
			"grup_baru", out.CreatedGroups, "grup_dihapus", out.RemovedGroups, "bentrok", out.Conflicts, "gagal", out.Failures)
	}
	return out, nil
}

// endSessions menghentikan semua proses milik UID (sesi SMB berjalan sebagai
// pengguna itu), agar pencabutan akses langsung berlaku.
func (a *Accounts) endSessions(ctx context.Context, uid int) {
	if !a.inRange(uid) {
		return
	}
	_, _ = runTool(ctx, a.t.Timeout, a.t.Pkill, []string{"-KILL", "--euid", strconv.Itoa(uid)})
}

// smb menjalankan smbpasswd dengan satu opsi untuk akun (bila Samba
// terpasang).
func (a *Accounts) smb(ctx context.Context, flag, name string) error {
	if _, err := os.Stat(a.t.Smbpasswd); err != nil {
		return nil
	}
	return a.run(ctx, a.t.Smbpasswd, []string{flag}, name)
}

// SMBPasswordOp mengembalikan handler smb.set_password: password Samba
// (hash NT disimpan Samba sendiri) disamakan dengan password panel. Hanya
// untuk akun buatan Sapo; password dikirim lewat stdin.
func (a *Accounts) SMBPasswordOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.SMBPasswordParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		if !accountNamePattern.MatchString(req.Name) || !validSMBPassword(req.Password) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "parameter tidak valid"}
		}
		if _, err := os.Stat(a.t.Smbpasswd); err != nil {
			return nil, &OpError{Code: proto.ErrUnavailable, Message: "Samba belum terpasang"}
		}
		// Sinkronisasi panjang tidak boleh menahan banyak permintaan ini.
		if !tryLockFor(ctx, &a.mu, 10*time.Second) {
			return nil, &OpError{Code: proto.ErrBusy, Message: "akun sedang disinkronkan"}
		}
		defer a.mu.Unlock()
		st, err := a.loadState()
		if err != nil {
			return nil, err
		}
		users, err := readPasswd(a.t.PasswdFile)
		if err != nil {
			return nil, err
		}
		s, rec := st.Users[req.Name]
		e, exists := users[req.Name]
		if !rec || !exists || e.UID != s.UID || e.GID != st.Primary || !a.inRange(e.UID) {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "akun " + req.Name + " bukan akun Sapo"}
		}
		input := []byte(req.Password + "\n" + req.Password + "\n")
		res, err := runToolInput(ctx, a.t.Timeout, a.t.Smbpasswd, input, []string{"-s", "-a"}, req.Name)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, errors.New("smbpasswd gagal: " + firstLine(res.Stderr))
		}
		// Mengganti password bisa mengaktifkan akun Samba lagi: status
		// nonaktif yang tercatat diterapkan ulang.
		if s.SMBDisabled != nil && *s.SMBDisabled {
			if err := a.run(ctx, a.t.Smbpasswd, []string{"-d"}, req.Name); err != nil {
				return nil, err
			}
		}
		return map[string]bool{"ok": true}, nil
	}
}

// validSMBPassword: aturan yang sama dengan panel (tanpa karakter kendali,
// paling panjang MaxSMBPassword byte).
func validSMBPassword(p string) bool {
	if p == "" || len(p) > proto.MaxSMBPassword {
		return false
	}
	return !strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// tryLockFor menunggu kunci paling lama d.
func tryLockFor(ctx context.Context, mu *sync.Mutex, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if mu.TryLock() {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
