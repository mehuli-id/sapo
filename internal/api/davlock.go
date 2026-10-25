package api

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// Kunci WebDAV (LOCK/UNLOCK) disimpan di memori sapo-api. Tabel kunci
// webdav dipakai bersama, tetapi:
//   - path @home diterjemahkan per pengguna (/~<nama>/…), jadi folder
//     pribadi pengguna lain tidak pernah bertabrakan;
//   - token yang dilihat klien acak dan hanya berlaku untuk pemiliknya
//     (token bawaan webdav berurutan dan bisa ditebak);
//   - lama kunci paling lama davLockMax dan jumlahnya dibatasi per pengguna.

const (
	davLockMax     = time.Hour
	davLocksPerUsr = 200
)

type davLockEntry struct {
	inner   string
	user    string
	root    string
	expires time.Time
}

// davLockTable: token klien → kunci di tabel webdav.
type davLockTable struct {
	inner webdav.LockSystem
	mu    sync.Mutex
	locks map[string]*davLockEntry
}

func newDAVLockTable() *davLockTable {
	return &davLockTable{inner: webdav.NewMemLS(), locks: map[string]*davLockEntry{}}
}

// forUser mengembalikan LockSystem untuk satu permintaan pengguna.
func (t *davLockTable) forUser(user string) webdav.LockSystem { return &davUserLocks{t: t, user: user} }

type davUserLocks struct {
	t    *davLockTable
	user string
}

// key: nama di tabel webdav. @home milik pengguna ini sendiri.
func (l *davUserLocks) key(name string) string {
	if name == "/"+davHomeName || strings.HasPrefix(name, "/"+davHomeName+"/") {
		return "/~" + l.user + strings.TrimPrefix(name, "/"+davHomeName)
	}
	return name
}

// innerToken: token webdav untuk token klien milik pengguna ini; token
// lain (milik pengguna lain atau tidak dikenal) tidak pernah cocok.
func (l *davUserLocks) innerToken(tok string) string {
	l.t.mu.Lock()
	defer l.t.mu.Unlock()
	if e, ok := l.t.locks[tok]; ok && e.user == l.user {
		return e.inner
	}
	return "sapo-invalid:" + tok
}

func (l *davUserLocks) Confirm(now time.Time, name0, name1 string, conditions ...webdav.Condition) (func(), error) {
	conds := make([]webdav.Condition, len(conditions))
	for i, c := range conditions {
		if c.Token != "" {
			c.Token = l.innerToken(c.Token)
		}
		conds[i] = c
	}
	if name0 != "" {
		name0 = l.key(name0)
	}
	if name1 != "" {
		name1 = l.key(name1)
	}
	return l.t.inner.Confirm(now, name0, name1, conds...)
}

func capDuration(d time.Duration) time.Duration {
	if d <= 0 || d > davLockMax {
		return davLockMax
	}
	return d
}

func (l *davUserLocks) Create(now time.Time, d webdav.LockDetails) (string, error) {
	l.t.mu.Lock()
	n := 0
	for tok, e := range l.t.locks {
		if now.After(e.expires) {
			delete(l.t.locks, tok)
			continue
		}
		if e.user == l.user {
			n++
		}
	}
	l.t.mu.Unlock()
	if n >= davLocksPerUsr {
		return "", webdav.ErrLocked
	}
	root := d.Root
	d.Root = l.key(d.Root)
	d.Duration = capDuration(d.Duration)
	inner, err := l.t.inner.Create(now, d)
	if err != nil {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		_ = l.t.inner.Unlock(now, inner)
		return "", err
	}
	tok := "opaquelocktoken:sapo-" + hex.EncodeToString(b)
	l.t.mu.Lock()
	l.t.locks[tok] = &davLockEntry{inner: inner, user: l.user, root: root, expires: now.Add(d.Duration)}
	l.t.mu.Unlock()
	return tok, nil
}

func (l *davUserLocks) entry(tok string) (*davLockEntry, bool) {
	l.t.mu.Lock()
	defer l.t.mu.Unlock()
	e, ok := l.t.locks[tok]
	if !ok || e.user != l.user {
		return nil, false
	}
	return e, true
}

func (l *davUserLocks) Refresh(now time.Time, tok string, d time.Duration) (webdav.LockDetails, error) {
	e, ok := l.entry(tok)
	if !ok {
		return webdav.LockDetails{}, webdav.ErrNoSuchLock
	}
	d = capDuration(d)
	det, err := l.t.inner.Refresh(now, e.inner, d)
	if err != nil {
		return webdav.LockDetails{}, err
	}
	l.t.mu.Lock()
	e.expires = now.Add(d)
	l.t.mu.Unlock()
	det.Root = e.root
	return det, nil
}

func (l *davUserLocks) Unlock(now time.Time, tok string) error {
	e, ok := l.entry(tok)
	if !ok {
		return webdav.ErrNoSuchLock
	}
	err := l.t.inner.Unlock(now, e.inner)
	l.t.mu.Lock()
	delete(l.t.locks, tok)
	l.t.mu.Unlock()
	return err
}
