package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
// menyentuh akun dan grup yang ia kelola: UID/GID di dalam rentang
// [IDMin, IDMax], dan untuk akun, grup utamanya PrimaryGroup. Akun lain
// (root, akun paket, akun buatan admin lewat terminal) tidak pernah diubah.
type AccountTools struct {
	Useradd, Userdel, Groupadd, Groupdel, Gpasswd string
	Smbpasswd                                     string
	PasswdFile, GroupFile                         string
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

// rangeKeys membatasi useradd/groupadd ke rentang Sapo.
func (a *Accounts) rangeKeys(kind string) []string {
	return []string{
		"--key", kind + "_MIN=" + strconv.Itoa(a.t.IDMin),
		"--key", kind + "_MAX=" + strconv.Itoa(a.t.IDMax),
	}
}

func (a *Accounts) run(ctx context.Context, prog string, flags []string, operands ...string) error {
	res, err := runTool(ctx, a.t.Timeout, prog, flags, operands...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s keluar dengan kode %d: %s", prog, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// SyncOp mengembalikan handler accounts.sync: menyamakan akun dan grup
// sistem yang dikelola Sapo dengan daftar pengguna dan grup di panel.
// Operasi ini deklaratif dan aman diulang. Akun yang tidak lagi ada di
// daftar dihapus (tanpa menghapus file miliknya).
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
	out := proto.AccountsSyncResult{Conflicts: []string{}}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	groups, err := readGroup(a.t.GroupFile)
	if err != nil {
		return out, err
	}
	// Grup utama penanda akun Sapo.
	pg, ok := groups[a.t.PrimaryGroup]
	if !ok {
		if err := a.run(ctx, a.t.Groupadd, a.rangeKeys("GID"), a.t.PrimaryGroup); err != nil {
			return out, err
		}
		if groups, err = readGroup(a.t.GroupFile); err != nil {
			return out, err
		}
		pg = groups[a.t.PrimaryGroup]
	}
	if !a.inRange(pg.GID) {
		return out, &OpError{Code: proto.ErrConflict, Message: fmt.Sprintf("grup %s (GID %d) di luar rentang akun Sapo", a.t.PrimaryGroup, pg.GID)}
	}
	users, err := readPasswd(a.t.PasswdFile)
	if err != nil {
		return out, err
	}
	managedUser := func(e passwdEntry) bool { return a.inRange(e.UID) && e.GID == pg.GID }
	managedGroup := func(e groupEntry) bool { return a.inRange(e.GID) && e.Name != a.t.PrimaryGroup }

	// 1. Grup yang dibutuhkan.
	wantGroups := map[string]bool{}
	for _, g := range req.Groups {
		wantGroups[g] = true
		e, exists := groups[g]
		switch {
		case !exists:
			if err := a.run(ctx, a.t.Groupadd, a.rangeKeys("GID"), g); err != nil {
				return out, err
			}
			out.CreatedGroups = append(out.CreatedGroups, g)
		case !managedGroup(e):
			// Nama sudah dipakai grup sistem: tidak pernah diambil alih.
			out.Conflicts = append(out.Conflicts, "group:"+g)
			wantGroups[g] = false
		}
	}

	// 2. Akun yang dibutuhkan.
	wantUsers := map[string]bool{}
	for _, u := range req.Users {
		wantUsers[u.Name] = true
		e, exists := users[u.Name]
		switch {
		case !exists:
			flags := []string{"--no-create-home", "--home-dir", "/nonexistent", "--shell", a.t.Shell, "--gid", a.t.PrimaryGroup, "--no-user-group"}
			if err := a.run(ctx, a.t.Useradd, append(flags, a.rangeKeys("UID")...), u.Name); err != nil {
				return out, err
			}
			out.CreatedUsers = append(out.CreatedUsers, u.Name)
		case !managedUser(e):
			out.Conflicts = append(out.Conflicts, "user:"+u.Name)
			wantUsers[u.Name] = false
		}
	}

	// 3. Anggota setiap grup yang dikelola (daftar lengkap).
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
	for g, ok := range wantGroups {
		if !ok {
			continue
		}
		want := members[g]
		slices.Sort(want)
		have := slices.Clone(groups[g].Members)
		slices.Sort(have)
		if !slices.Equal(want, have) {
			if err := a.run(ctx, a.t.Gpasswd, []string{"--members", strings.Join(want, ",")}, g); err != nil {
				return out, err
			}
		}
	}

	// 4. Akun dan grup Sapo yang sudah tidak ada di panel dihapus. File
	//    miliknya tetap ada (pemiliknya menjadi angka UID).
	if users, err = readPasswd(a.t.PasswdFile); err != nil {
		return out, err
	}
	for name, e := range users {
		if managedUser(e) && !wantUsers[name] && !slices.Contains(out.Conflicts, "user:"+name) {
			a.dropSMB(ctx, name)
			if err := a.run(ctx, a.t.Userdel, nil, name); err != nil {
				return out, err
			}
			out.RemovedUsers = append(out.RemovedUsers, name)
		}
	}
	for name, e := range groups {
		if managedGroup(e) && !wantGroups[name] && !slices.Contains(out.Conflicts, "group:"+name) {
			if err := a.run(ctx, a.t.Groupdel, nil, name); err != nil {
				return out, err
			}
			out.RemovedGroups = append(out.RemovedGroups, name)
		}
	}

	// 5. Akun nonaktif tidak bisa masuk lewat SMB.
	for _, u := range req.Users {
		if wantUsers[u.Name] {
			a.setSMBEnabled(ctx, u.Name, !u.Disabled)
		}
	}
	if len(out.CreatedUsers)+len(out.RemovedUsers)+len(out.CreatedGroups)+len(out.RemovedGroups)+len(out.Conflicts) > 0 && a.t.Log != nil {
		a.t.Log.Info("akun sistem disinkronkan", "akun_baru", out.CreatedUsers, "akun_dihapus", out.RemovedUsers,
			"grup_baru", out.CreatedGroups, "grup_dihapus", out.RemovedGroups, "bentrok", out.Conflicts)
	}
	return out, nil
}

// dropSMB menghapus akun dari basis data Samba (bila Samba terpasang).
func (a *Accounts) dropSMB(ctx context.Context, name string) {
	if _, err := os.Stat(a.t.Smbpasswd); err != nil {
		return
	}
	_, _ = runTool(ctx, a.t.Timeout, a.t.Smbpasswd, []string{"-x"}, name)
}

// setSMBEnabled mengaktifkan atau menonaktifkan akun Samba yang sudah ada.
// Akun yang belum punya password Samba diabaikan (dibuat saat password
// disinkronkan).
func (a *Accounts) setSMBEnabled(ctx context.Context, name string, enabled bool) {
	if _, err := os.Stat(a.t.Smbpasswd); err != nil {
		return
	}
	flag := "-e"
	if !enabled {
		flag = "-d"
	}
	_, _ = runTool(ctx, a.t.Timeout, a.t.Smbpasswd, []string{flag}, name)
}

// SMBPasswordOp mengembalikan handler smb.set_password: password Samba
// (hash NT disimpan Samba sendiri) disamakan dengan password panel. Hanya
// untuk akun yang dikelola Sapo; password dikirim lewat stdin.
func (a *Accounts) SMBPasswordOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req proto.SMBPasswordParams
		if err := decodeParams(raw, &req); err != nil {
			return nil, err
		}
		if !accountNamePattern.MatchString(req.Name) || req.Password == "" || len(req.Password) > 1024 ||
			strings.ContainsAny(req.Password, "\n\r\x00") {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "parameter tidak valid"}
		}
		if _, err := os.Stat(a.t.Smbpasswd); err != nil {
			return nil, &OpError{Code: proto.ErrUnavailable, Message: "Samba belum terpasang"}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		users, err := readPasswd(a.t.PasswdFile)
		if err != nil {
			return nil, err
		}
		groups, err := readGroup(a.t.GroupFile)
		if err != nil {
			return nil, err
		}
		e, ok := users[req.Name]
		pg, pgOK := groups[a.t.PrimaryGroup]
		if !ok || !pgOK || !a.inRange(e.UID) || e.GID != pg.GID {
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
		return map[string]bool{"ok": true}, nil
	}
}
