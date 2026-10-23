package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Role adalah peran user. Izin per modul akan disimpan sebagai data; untuk
// tahap ini baru ada admin.
type Role string

// RoleAdmin adalah administrator penuh.
const RoleAdmin Role = "admin"

// User adalah akun panel.
type User struct {
	Username     string    `json:"username"`
	Role         Role      `json:"role"`
	PasswordHash string    `json:"password_hash"`
	TOTPSecret   string    `json:"totp_secret,omitempty"`
	TOTPLastStep int64     `json:"totp_last_step,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// State adalah isi file penyimpanan.
type State struct {
	Version          int        `json:"version"`
	SetupCompletedAt *time.Time `json:"setup_completed_at,omitempty"`
	Language         string     `json:"language,omitempty"`
	Users            []User     `json:"users"`
}

// ErrSetupDone dikembalikan bila setup awal sudah pernah selesai.
var ErrSetupDone = errors.New("setup awal sudah selesai")

// Store menyimpan user di satu file JSON (izin 0600) dengan penulisan atomik.
//
// Ini penyimpanan sementara untuk Fase 1 (data sangat kecil). Antarmukanya
// sengaja sempit agar bisa diganti SQLite tanpa mengubah pemanggilnya.
type Store struct {
	path  string
	mu    sync.Mutex
	state State
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
// hanya berhasil bila step lebih baru dari yang tersimpan.
func (s *Store) ConsumeTOTPStep(username string, step int64) error {
	return s.updateUser(username, func(u *User) error {
		if step <= u.TOTPLastStep {
			return ErrTOTPReused
		}
		u.TOTPLastStep = step
		return nil
	})
}

// SetPasswordHash mengganti hash password (misalnya setelah parameter
// Argon2 di konfigurasi dinaikkan).
func (s *Store) SetPasswordHash(username, hash string) error {
	return s.updateUser(username, func(u *User) error {
		u.PasswordHash = hash
		return nil
	})
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
	return errors.New("user tidak ditemukan")
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
