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
	"syscall"
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
	Smbpasswd, Smbstatus                          string
	// ProcDir: /proc (diganti di test).
	ProcDir               string
	PasswdFile, GroupFile string
	StateFile             string
	IDMin, IDMax          int
	PrimaryGroup          string
	Shell                 string
	Timeout               time.Duration
	Log                   *slog.Logger
}

// Accounts melayani accounts.sync dan smb.set_password. Hanya satu
// perubahan akun yang berjalan pada satu waktu.
type Accounts struct {
	t  AccountTools
	mu sync.Mutex
	// kill menghentikan proses (diganti di test).
	kill func(pid int) error
}

// NewAccounts menyiapkan operasi akun.
func NewAccounts(t AccountTools) *Accounts {
	if t.ProcDir == "" {
		t.ProcDir = "/proc"
	}
	return &Accounts{t: t, kill: func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }}
}

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

// Managed mengembalikan UID dan GID akun name bila akun itu buatan sapod:
// tercatat dengan UID yang sama, di rentang ID, dan bergrup utama Sapo.
func (a *Accounts) Managed(name string) (uid, gid int, ok bool) {
	st, err := a.loadState()
	if err != nil {
		return 0, 0, false
	}
	users, err := readPasswd(a.t.PasswdFile)
	if err != nil {
		return 0, 0, false
	}
	groups, err := readGroup(a.t.GroupFile)
	if err != nil {
		return 0, 0, false
	}
	pg, hasPG := groups[a.t.PrimaryGroup]
	rec, isRec := st.Users[name]
	e, exists := users[name]
	if !hasPG || pg.GID != st.Primary || !a.inRange(pg.GID) || !isRec || !exists || e.UID != rec.UID || e.GID != pg.GID || !a.inRange(e.UID) {
		return 0, 0, false
	}
	return e.UID, e.GID, true
}

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
		// ID dicatat sebelum groupadd: bila sapod berhenti di tengah, grup
		// tetap dikenali sebagai buatan Sapo pada percobaan berikutnya.
		id := st.Primary
		if id == 0 {
			if id, err = a.allocID(&st, users, groups); err != nil {
				return out, err
			}
			st.Primary = id
			if err := a.saveState(st); err != nil {
				return out, err
			}
		}
		if err := a.run(ctx, a.t.Groupadd, []string{"--gid", strconv.Itoa(id)}, a.t.PrimaryGroup); err != nil {
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
			id, rec := st.Groups[g]
			if !rec {
				if id, err = a.allocID(&st, users, groups); err != nil {
					fail("group:"+g, err)
					continue
				}
				st.Groups[g] = id
				if err := a.saveState(st); err != nil {
					return out, err
				}
			}
			if err := a.run(ctx, a.t.Groupadd, []string{"--gid", strconv.Itoa(id)}, g); err != nil {
				fail("group:"+g, err)
				continue
			}
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
			rec, ok := st.Users[u.Name]
			id := rec.UID
			if !ok {
				if id, err = a.allocID(&st, users, groups); err != nil {
					fail("user:"+u.Name, err)
					continue
				}
				st.Users[u.Name] = stateAccount{UID: id}
				if err := a.saveState(st); err != nil {
					return out, err
				}
			}
			flags := []string{"--uid", strconv.Itoa(id), "--no-create-home", "--home-dir", "/nonexistent", "--shell", a.t.Shell, "--gid", strconv.Itoa(pg.GID), "--no-user-group"}
			if err := a.run(ctx, a.t.Useradd, flags, u.Name); err != nil {
				fail("user:"+u.Name, err)
				continue
			}
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
				continue
			}
			// Proses lama tetap membawa keanggotaan grup lama: sesi anggota
			// yang dikeluarkan diputus agar izin grup langsung dicabut.
			for _, m := range have {
				if !slices.Contains(want, m) {
					if e, ok := users[m]; ok {
						a.endSessions(ctx, m, e.UID)
					}
				}
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
			a.endSessions(ctx, name, users[name].UID)
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
		// Akun yang belum punya password Samba tidak bisa masuk lewat SMB;
		// statusnya diterapkan saat password Samba dibuat (SMBPasswordOp).
		flag := "-e"
		if u.Disabled {
			flag = "-d"
		}
		if err := a.smb(ctx, flag, u.Name); err != nil && !smbNoEntry(err) {
			fail("user:"+u.Name, err)
			continue
		}
		if u.Disabled {
			a.endSessions(ctx, u.Name, s.UID)
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

// endSessions memutus sesi SMB pengguna agar pencabutan akses langsung
// berlaku: proses smbd yang melayani pengguna itu (dari smbstatus) dan
// semua proses yang berjalan dengan UID-nya (real, efektif, tersimpan, atau
// sistem berkas). Hanya UID akun Sapo yang pernah dihentikan.
func (a *Accounts) endSessions(ctx context.Context, name string, uid int) {
	if !a.inRange(uid) {
		return
	}
	pids := map[int]bool{}
	for _, pid := range a.smbSessionPIDs(ctx, name) {
		// Hanya proses smbd (bukan PID lain yang kebetulan sama).
		if comm, err := os.ReadFile(filepath.Join(a.t.ProcDir, strconv.Itoa(pid), "comm")); err == nil && strings.TrimSpace(string(comm)) == "smbd" {
			pids[pid] = true
		}
	}
	entries, _ := os.ReadDir(a.t.ProcDir)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(a.t.ProcDir, e.Name(), "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if f, ok := strings.CutPrefix(line, "Uid:"); ok && slices.Contains(strings.Fields(f), strconv.Itoa(uid)) {
				pids[pid] = true
			}
		}
	}
	for pid := range pids {
		_ = a.kill(pid)
	}
	if len(pids) > 0 && a.t.Log != nil {
		a.t.Log.Info("sesi pengguna diputus", "user", name, "proses", len(pids))
	}
}

// smbSessionPIDs membaca proses smbd milik sesi pengguna (smbstatus --json).
func (a *Accounts) smbSessionPIDs(ctx context.Context, name string) []int {
	if _, err := os.Stat(a.t.Smbstatus); err != nil {
		return nil
	}
	res, err := runTool(ctx, a.t.Timeout, a.t.Smbstatus, []string{"--processes", "--json"})
	if err != nil || res.ExitCode != 0 {
		return nil
	}
	var st struct {
		Sessions map[string]struct {
			Username string `json:"username"`
			ServerID struct {
				PID flexUint `json:"pid"`
			} `json:"server_id"`
		} `json:"sessions"`
	}
	if json.Unmarshal(res.Stdout, &st) != nil {
		return nil
	}
	var out []int
	for _, sess := range st.Sessions {
		// Nama di Samba bisa berawalan nama mesin (MESIN\nama).
		user := sess.Username
		if i := strings.LastIndexByte(user, '\\'); i >= 0 {
			user = user[i+1:]
		}
		if strings.EqualFold(user, name) && sess.ServerID.PID > 1 {
			out = append(out, int(sess.ServerID.PID))
		}
	}
	return out
}

// smbNoEntry: smbpasswd gagal karena akun belum ada di basis data Samba.
func smbNoEntry(err error) bool {
	m := strings.ToLower(err.Error())
	// "Failed to find entry for user x" (versi lama) atau "Failed to find
	// user x in passdb backend" (Samba 4.2x).
	return strings.Contains(m, "failed to find entry") || strings.Contains(m, "failed to find user") ||
		strings.Contains(m, "does not exist") || strings.Contains(m, "not found")
}

// smb menjalankan smbpasswd dengan satu opsi untuk akun (bila Samba
// terpasang). Pesan error memuat seluruh keluaran (pesan "akun belum ada"
// tidak selalu di baris pertama).
func (a *Accounts) smb(ctx context.Context, flag, name string) error {
	if _, err := os.Stat(a.t.Smbpasswd); err != nil {
		return nil
	}
	res, err := runTool(ctx, a.t.Timeout, a.t.Smbpasswd, []string{flag}, name)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		out := strings.Join(strings.Fields(string(res.Stderr)+" "+string(res.Stdout)), " ")
		return fmt.Errorf("smbpasswd %s keluar dengan kode %d: %s", flag, res.ExitCode, clipTo(out, maxErrorText))
	}
	return nil
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
				// Gagal menonaktifkan: akun Samba dihapus (gagal-tertutup).
				_ = a.run(ctx, a.t.Smbpasswd, []string{"-x"}, req.Name)
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
