package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// Role adalah peran user. Izin per folder dan aplikasi diatur lewat grup.
type Role string

const (
	// RoleAdmin adalah administrator penuh.
	RoleAdmin Role = "admin"
	// RoleUser adalah pengguna biasa: mengakses file dan aplikasi yang
	// diizinkan, tidak bisa mengubah pengaturan sistem.
	RoleUser Role = "user"
)

// ValidRole memeriksa nilai peran dari luar (panel).
func ValidRole(r Role) bool { return r == RoleAdmin || r == RoleUser }

// User adalah akun panel.
type User struct {
	Username     string `json:"username"`
	Role         Role   `json:"role"`
	PasswordHash string `json:"password_hash"`
	TOTPSecret   string `json:"totp_secret,omitempty"`
	TOTPLastStep int64  `json:"totp_last_step,omitempty"`
	// Hash kode cadangan 2FA yang belum dipakai (lihat recovery.go).
	RecoveryCodes []string  `json:"recovery_codes,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	// Description: keterangan bebas dari admin (misalnya nama lengkap).
	Description string `json:"description,omitempty"`
	// Disabled: akun dinonaktifkan; tidak bisa login, sesinya diakhiri.
	Disabled bool `json:"disabled,omitempty"`
	// MustChangePassword: password dibuat atau di-reset admin, jadi pemilik
	// akun wajib menggantinya sebelum memakai panel.
	MustChangePassword bool `json:"must_change_password,omitempty"`
	// Groups: nama grup yang diikuti user.
	Groups      []string   `json:"groups,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	// AppPasswords: password aplikasi untuk WebDAV (apppassword.go).
	AppPasswords []AppPassword `json:"app_passwords,omitempty"`
}

// Group adalah kumpulan user, dipakai untuk izin folder dan aplikasi.
type Group struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// State adalah isi file penyimpanan.
type State struct {
	Version          int        `json:"version"`
	SetupCompletedAt *time.Time `json:"setup_completed_at,omitempty"`
	Language         string     `json:"language,omitempty"`
	Users            []User     `json:"users"`
	Groups           []Group    `json:"groups,omitempty"`
}

// ErrSetupDone dikembalikan bila setup awal sudah pernah selesai.
var ErrSetupDone = errors.New("setup awal sudah selesai")

// Store menyimpan user di satu file JSON (izin 0600) dengan penulisan atomik.
//
// Ini penyimpanan sementara untuk Fase 1 (data sangat kecil). Antarmukanya
// sengaja sempit agar bisa diganti SQLite tanpa mengubah pemanggilnya.
type Store struct {
	// RequireTOTPForAdmin: administrator tanpa 2FA belum dihitung sebagai
	// administrator aktif (ia belum bisa memakai panel).
	RequireTOTPForAdmin bool

	path  string
	mu    sync.Mutex
	state State
	// touching: password aplikasi yang waktu pakainya sedang dicatat;
	// touchWG menunggu semuanya (test).
	touching sync.Map
	touchWG  sync.WaitGroup
}

// OpenStore membuka (atau membuat) penyimpanan di dir.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("membuat direktori data: %w", err)
	}
	s := &Store{path: filepath.Join(dir, "users.json"), state: State{Version: 1}}
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("file user rusak (%s): %w", s.path, err)
	}
	return s, nil
}

// SetupRequired benar bila belum ada admin.
func (s *Store) SetupRequired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.SetupCompletedAt == nil
}

// Language mengembalikan bahasa yang dipilih saat setup.
func (s *Store) Language() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Language
}

// CompleteSetup membuat admin pertama. Hanya berhasil sekali.
func (s *Store) CompleteSetup(admin User, language string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.SetupCompletedAt != nil {
		return ErrSetupDone
	}
	next := s.state
	next.Users = append(append([]User(nil), s.state.Users...), admin)
	next.Language = language
	next.SetupCompletedAt = &now
	if err := s.write(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

// Get mengembalikan salinan user.
func (s *Store) Get(username string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.state.Users {
		if u.Username == username {
			return u, true
		}
	}
	return User{}, false
}

// ErrTOTPReused dikembalikan bila kode TOTP dari langkah waktu yang sama
// atau lebih lama sudah pernah dipakai (termasuk dua login bersamaan).
var ErrTOTPReused = errors.New("kode 2FA sudah dipakai")

// ConsumeTOTPStep mencatat step TOTP yang dipakai secara compare-and-set:
// hanya berhasil bila step lebih baru dari yang tersimpan DAN rahasia 2FA
// masih sama dengan yang dipakai memeriksa kode (rahasia bisa diganti dari
// panel di antara pemeriksaan password dan kode).
func (s *Store) ConsumeTOTPStep(username, secret string, step int64) error {
	return s.updateUser(username, func(u *User) error {
		if step <= u.TOTPLastStep || u.TOTPSecret != secret {
			return ErrTOTPReused
		}
		u.TOTPLastStep = step
		return nil
	})
}

// ConsumeRecoveryCode memakai satu kode cadangan: bila cocok, hash-nya
// dihapus (sekali pakai) dan jumlah kode yang tersisa dikembalikan. Dua login
// bersamaan dengan kode yang sama: hanya satu yang berhasil.
func (s *Store) ConsumeRecoveryCode(username, code string) (int, error) {
	remaining := 0
	err := s.updateUser(username, func(u *User) error {
		i := matchRecovery(u.RecoveryCodes, code)
		if i < 0 {
			return ErrRecoveryInvalid
		}
		u.RecoveryCodes = append(append([]string(nil), u.RecoveryCodes[:i]...), u.RecoveryCodes[i+1:]...)
		remaining = len(u.RecoveryCodes)
		return nil
	})
	return remaining, err
}

// SetRecoveryCodes mengganti semua kode cadangan (kode lama tidak berlaku).
func (s *Store) SetRecoveryCodes(username string, hashes []string) error {
	return s.updateUser(username, func(u *User) error {
		u.RecoveryCodes = append([]string(nil), hashes...)
		return nil
	})
}

// SetTOTP mengganti rahasia 2FA (misalnya pindah ke HP baru). step adalah
// langkah waktu kode yang dipakai untuk konfirmasi, agar tidak bisa dipakai ulang.
func (s *Store) SetTOTP(username, secret string, step int64) error {
	return s.updateUser(username, func(u *User) error {
		u.TOTPSecret = secret
		u.TOTPLastStep = step
		return nil
	})
}

// ErrPasswordChanged: hash password sudah berbeda dari yang diharapkan.
var ErrPasswordChanged = errors.New("password sudah diganti")

// ReplacePasswordHash mengganti hash password hanya bila hash yang tersimpan
// masih old (compare-and-set), misalnya hash ulang setelah parameter Argon2
// dinaikkan: tidak boleh menimpa password yang baru di-reset.
func (s *Store) ReplacePasswordHash(username, old, hash string) error {
	return s.updateUser(username, func(u *User) error {
		if u.PasswordHash != old {
			return ErrPasswordChanged
		}
		u.PasswordHash = hash
		return nil
	})
}

// Kesalahan pengelolaan user dan grup.
var (
	ErrUserExists   = errors.New("nama pengguna sudah dipakai")
	ErrUserNotFound = errors.New("user tidak ditemukan")
	// ErrLastAdmin: perubahan akan membuat NAS tanpa administrator aktif.
	ErrLastAdmin     = errors.New("harus tetap ada minimal satu administrator aktif")
	ErrGroupExists   = errors.New("nama grup sudah dipakai")
	ErrGroupNotFound = errors.New("grup tidak ditemukan")
	ErrNotAdmin      = errors.New("bukan administrator")
)

// Users mengembalikan salinan semua user, urut nama.
func (s *Store) Users() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := cloneUsers(s.state.Users)
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// Groups mengembalikan salinan semua grup, urut nama.
func (s *Store) Groups() []Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Group(nil), s.state.Groups...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CreateUser menambah user baru. Grupnya harus sudah ada.
func (s *Store) CreateUser(u User) error {
	return s.mutate(func(st *State) error {
		for _, x := range st.Users {
			if x.Username == u.Username {
				return ErrUserExists
			}
		}
		if err := checkGroups(st, u.Groups); err != nil {
			return err
		}
		u.Groups = uniqueSorted(u.Groups)
		st.Users = append(st.Users, u)
		return nil
	})
}

// UserChange adalah perubahan atribut user oleh admin; nil = tidak diubah.
type UserChange struct {
	Role        *Role
	Disabled    *bool
	Groups      *[]string
	Description *string
}

// UpdateUser menerapkan perubahan. Administrator aktif terakhir tidak bisa
// diturunkan perannya atau dinonaktifkan.
func (s *Store) UpdateUser(username string, c UserChange) error {
	return s.mutate(func(st *State) error {
		u := findUser(st, username)
		if u == nil {
			return ErrUserNotFound
		}
		if c.Groups != nil {
			if err := checkGroups(st, *c.Groups); err != nil {
				return err
			}
			u.Groups = uniqueSorted(*c.Groups)
		}
		if c.Role != nil {
			u.Role = *c.Role
		}
		if c.Disabled != nil {
			u.Disabled = *c.Disabled
		}
		if c.Description != nil {
			u.Description = *c.Description
		}
		return requireActiveAdmin(st, s.RequireTOTPForAdmin)
	})
}

// SetPassword mengganti hash password. mustChange: pemilik akun wajib
// menggantinya lagi saat login (password dibuat admin).
func (s *Store) SetPassword(username, hash string, mustChange bool) error {
	return s.updateUser(username, func(u *User) error {
		u.PasswordHash = hash
		u.MustChangePassword = mustChange
		// Password diganti atau di-reset: password aplikasi ikut dicabut.
		u.AppPasswords = nil
		return nil
	})
}

// RecoveryReset dipakai menu darurat di konsol NAS (sapo-console) saat
// password dan kode cadangan admin hilang: password diganti hash baru dan
// wajib diganti saat login, 2FA dan kode cadangan dihapus (didaftarkan ulang
// saat login), dan akun diaktifkan kembali. Hanya untuk administrator.
func (s *Store) RecoveryReset(username, hash string) error {
	return s.updateUser(username, func(u *User) error {
		if u.Role != RoleAdmin {
			return ErrNotAdmin
		}
		u.PasswordHash, u.MustChangePassword, u.Disabled = hash, true, false
		u.TOTPSecret, u.TOTPLastStep, u.RecoveryCodes = "", 0, nil
		u.AppPasswords = nil
		return nil
	})
}

// RecordLogin mencatat waktu login terakhir.
func (s *Store) RecordLogin(username string, at time.Time) error {
	return s.updateUser(username, func(u *User) error {
		u.LastLoginAt = &at
		return nil
	})
}

// DeleteUser menghapus user. Administrator aktif terakhir tidak bisa dihapus.
func (s *Store) DeleteUser(username string) error {
	return s.mutate(func(st *State) error {
		for i, u := range st.Users {
			if u.Username == username {
				st.Users = append(st.Users[:i:i], st.Users[i+1:]...)
				return requireActiveAdmin(st, s.RequireTOTPForAdmin)
			}
		}
		return ErrUserNotFound
	})
}

// CreateGroup menambah grup.
func (s *Store) CreateGroup(g Group) error {
	return s.mutate(func(st *State) error {
		for _, x := range st.Groups {
			if x.Name == g.Name {
				return ErrGroupExists
			}
		}
		st.Groups = append(st.Groups, g)
		return nil
	})
}

// UpdateGroup mengganti keterangan grup dan, bila members tidak nil,
// anggotanya (user di luar daftar dikeluarkan dari grup).
func (s *Store) UpdateGroup(name string, description *string, members *[]string) error {
	return s.mutate(func(st *State) error {
		g := findGroup(st, name)
		if g == nil {
			return ErrGroupNotFound
		}
		if description != nil {
			g.Description = *description
		}
		if members == nil {
			return nil
		}
		want := map[string]bool{}
		for _, m := range *members {
			if findUser(st, m) == nil {
				return ErrUserNotFound
			}
			want[m] = true
		}
		for i := range st.Users {
			u := &st.Users[i]
			groups := slices.DeleteFunc(slices.Clone(u.Groups), func(x string) bool { return x == name })
			if want[u.Username] {
				groups = append(groups, name)
			}
			u.Groups = uniqueSorted(groups)
		}
		return nil
	})
}

// DeleteGroup menghapus grup dan keanggotaannya.
func (s *Store) DeleteGroup(name string) error {
	return s.mutate(func(st *State) error {
		i := slices.IndexFunc(st.Groups, func(g Group) bool { return g.Name == name })
		if i < 0 {
			return ErrGroupNotFound
		}
		st.Groups = slices.Delete(st.Groups, i, i+1)
		for j := range st.Users {
			st.Users[j].Groups = slices.DeleteFunc(slices.Clone(st.Users[j].Groups), func(x string) bool { return x == name })
		}
		return nil
	})
}

// mutate menjalankan fn pada salinan state, lalu menyimpannya bila berhasil.
// Salinan dibuat dalam (slice di dalam user ikut disalin), jadi state lama
// tidak pernah berubah bila fn atau penulisan gagal.
func (s *Store) mutate(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state
	next.Users = cloneUsers(s.state.Users)
	next.Groups = append([]Group(nil), s.state.Groups...)
	if err := fn(&next); err != nil {
		return err
	}
	if err := s.write(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func cloneUsers(in []User) []User {
	out := make([]User, len(in))
	for i, u := range in {
		u.Groups = slices.Clone(u.Groups)
		u.RecoveryCodes = slices.Clone(u.RecoveryCodes)
		out[i] = u
	}
	return out
}

func findUser(st *State, name string) *User {
	for i := range st.Users {
		if st.Users[i].Username == name {
			return &st.Users[i]
		}
	}
	return nil
}

func findGroup(st *State, name string) *Group {
	for i := range st.Groups {
		if st.Groups[i].Name == name {
			return &st.Groups[i]
		}
	}
	return nil
}

func checkGroups(st *State, groups []string) error {
	for _, g := range groups {
		if findGroup(st, g) == nil {
			return ErrGroupNotFound
		}
	}
	return nil
}

func uniqueSorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// requireActiveAdmin: setelah setup, harus selalu ada administrator yang
// benar-benar bisa memakai panel (aktif, tanpa langkah wajib yang tertunda),
// agar NAS tidak terkunci dari panel.
func requireActiveAdmin(st *State, requireTOTP bool) error {
	if st.SetupCompletedAt == nil {
		return nil
	}
	for _, u := range st.Users {
		if u.Role == RoleAdmin && !u.Disabled && !u.MustChangePassword && (u.TOTPSecret != "" || !requireTOTP) {
			return nil
		}
	}
	return ErrLastAdmin
}

func (s *Store) updateUser(username string, fn func(*User) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state
	next.Users = append([]User(nil), s.state.Users...)
	for i := range next.Users {
		if next.Users[i].Username == username {
			if err := fn(&next.Users[i]); err != nil {
				return err
			}
			if err := s.write(next); err != nil {
				return err
			}
			s.state = next
			return nil
		}
	}
	return ErrUserNotFound
}

// write menulis state ke file sementara, fsync, lalu rename (atomik).
func (s *Store) write(st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".users-*.json")
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
	return os.Rename(tmp.Name(), s.path)
}
