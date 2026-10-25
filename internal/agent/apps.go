package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
	"gopkg.in/yaml.v3"
)

// App Center (ADR 0012): katalog kurasi, Podman + Quadlet, setiap aplikasi
// berjalan sebagai akun Linux sendiri tanpa capability.

// AppsTools mengatur operasi apps.* (agent.apps di sapo.yaml).
type AppsTools struct {
	CatalogDir, Podman, QuadletDir, StateFile string
	Systemctl                                 string
	Useradd, Userdel, Groupadd, Groupdel      string
	IDMin, IDMax                              int
	ReservedPorts                             []int
	MountBase                                 string
	Timeout                                   time.Duration
	Log                                       *slog.Logger
}

// Apps melayani App Center. Perubahan memakai kunci pool (dataset).
type Apps struct {
	t      AppsTools
	shares *Shares
	mu     sync.Mutex
	// chown dan portFree diganti di test.
	chown    func(path string, uid, gid int) error
	portFree func(port int) bool
}

// NewApps menyiapkan App Center.
func NewApps(t AppsTools, shares *Shares) *Apps {
	a := &Apps{t: t, shares: shares, chown: os.Lchown, portFree: portFree}
	// Pool yang masih berisi data aplikasi tidak dihapus (dicek paling awal,
	// sebelum ekspor NFS dicabut).
	shares.p.beforeDestroy = append([]func(context.Context, string) error{a.refusePool}, shares.p.beforeDestroy...)
	return a
}

// refusePool menolak menghapus pool yang masih menyimpan data aplikasi.
func (a *Apps) refusePool(_ context.Context, pool string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.load()
	if err != nil {
		return err
	}
	for id, s := range st.Apps {
		if s.HasData && s.Pool == pool {
			return &OpError{Code: proto.ErrConflict, Message: "pool " + pool + " masih berisi data aplikasi " + id + "; hapus aplikasi itu beserta datanya dulu di App Center"}
		}
	}
	return nil
}

// portFree: port TCP belum dipakai program lain di NAS.
func portFree(port int) bool {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// appDef adalah satu entri katalog (apps/<id>.yaml).
type appDef struct {
	ID          string            `yaml:"id"`
	Name        map[string]string `yaml:"name"`
	Description map[string]string `yaml:"description"`
	Website     string            `yaml:"website"`
	Image       string            `yaml:"image"`
	WebPort     int               `yaml:"web_port"`
	DefaultPort int               `yaml:"default_port"`
	DataPath    string            `yaml:"data_path"`
	Env         map[string]string `yaml:"env"`
	MemoryMB    int               `yaml:"memory_mb"`
}

var (
	appIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	appImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*(:[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}$`)
	appPathPattern  = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)
	appEnvKey       = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	appEnvValue     = regexp.MustCompile(`^[A-Za-z0-9._:/@+=,-]{0,256}$`)
)

func (d appDef) validate() error {
	switch {
	case !appIDPattern.MatchString(d.ID):
		return errors.New("id tidak valid")
	case !appImagePattern.MatchString(d.Image):
		return errors.New("image harus dikunci digest (repo:tag@sha256:…)")
	case d.WebPort < 1 || d.WebPort > 65535 || d.DefaultPort < 1024 || d.DefaultPort > 65535:
		return errors.New("port tidak valid")
	case !appPathPattern.MatchString(d.DataPath):
		return errors.New("data_path tidak valid")
	case d.MemoryMB < 32 || d.MemoryMB > 65536:
		return errors.New("memory_mb tidak valid")
	}
	for k, v := range d.Env {
		if !appEnvKey.MatchString(k) || !appEnvValue.MatchString(v) {
			return fmt.Errorf("env %s tidak valid", clip(k))
		}
	}
	return nil
}

// catalog membaca semua entri katalog yang valid.
func (a *Apps) catalog() (map[string]appDef, error) {
	entries, err := os.ReadDir(a.t.CatalogDir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]appDef{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]appDef{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(a.t.CatalogDir, e.Name()))
		if err != nil {
			continue
		}
		var d appDef
		if err := yaml.Unmarshal(data, &d); err != nil || d.validate() != nil || e.Name() != d.ID+".yaml" {
			if a.t.Log != nil {
				a.t.Log.Warn("entri katalog aplikasi tidak valid, dilewati", "file", e.Name())
			}
			continue
		}
		out[d.ID] = d
	}
	return out, nil
}

// appsState disimpan di StateFile.
type appsState struct {
	NextID int                  `json:"next_id"`
	Apps   map[string]*appState `json:"apps"`
}

type appState struct {
	UID         int       `json:"uid"`
	Pool        string    `json:"pool"`
	Port        int       `json:"port"`
	Installed   bool      `json:"installed"`
	Enabled     bool      `json:"enabled"`
	HasData     bool      `json:"has_data"`
	InstalledAt time.Time `json:"installed_at"`
}

func (a *Apps) load() (appsState, error) {
	st := appsState{NextID: a.t.IDMin, Apps: map[string]*appState{}}
	data, err := os.ReadFile(a.t.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("catatan aplikasi rusak (%s): %w", a.t.StateFile, err)
	}
	if st.Apps == nil {
		st.Apps = map[string]*appState{}
	}
	return st, nil
}

func (a *Apps) save(st appsState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(a.t.StateFile, data)
}

func appAccount(id string) string { return "sapo-app-" + id }
func appUnit(id string) string    { return "sapo-app-" + id + ".service" }

func (a *Apps) systemctl(ctx context.Context, flags []string, operands ...string) error {
	res, err := runTool(ctx, a.t.Timeout, a.t.Systemctl, flags, operands...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("systemctl %s: %s", strings.Join(flags, " "), firstLine(res.Stderr))
	}
	return nil
}

func (a *Apps) status(ctx context.Context, id string) string {
	res, err := runTool(ctx, a.t.Timeout, a.t.Systemctl, []string{"is-active"}, appUnit(id))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(res.Stdout))
}

// --- Handler -----------------------------------------------------------------

// CatalogOp mengembalikan handler apps.catalog.
func (a *Apps) CatalogOp() Handler {
	return func(context.Context, json.RawMessage) (any, error) {
		cat, err := a.catalog()
		if err != nil {
			return nil, err
		}
		out := []proto.CatalogApp{}
		for _, d := range cat {
			out = append(out, proto.CatalogApp{ID: d.ID, Name: d.Name, Description: d.Description, Website: d.Website,
				Image: d.Image, DefaultPort: d.DefaultPort, MemoryMB: d.MemoryMB})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, nil
	}
}

// ListOp mengembalikan handler apps.list.
func (a *Apps) ListOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		a.mu.Lock()
		st, err := a.load()
		a.mu.Unlock()
		if err != nil {
			return nil, err
		}
		out := []proto.InstalledApp{}
		for id, s := range st.Apps {
			if !s.Installed && !s.HasData {
				continue
			}
			app := proto.InstalledApp{ID: id, Pool: s.Pool, Port: s.Port, Installed: s.Installed, Enabled: s.Enabled, InstalledAt: s.InstalledAt}
			if s.Installed {
				app.Status = a.status(ctx, id)
			}
			out = append(out, app)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, nil
	}
}

func invalidApp(msg string) error { return &OpError{Code: proto.ErrInvalidParams, Message: msg} }

// InstallOp mengembalikan handler apps.install.
func (a *Apps) InstallOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.AppInstallParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		cat, err := a.catalog()
		if err != nil {
			return nil, err
		}
		def, ok := cat[p.ID]
		if !ok {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "aplikasi tidak ada di katalog"}
		}
		if !poolNamePattern.MatchString(p.Pool) {
			return nil, invalidApp("pool tidak valid")
		}
		if p.Port < 1024 || p.Port > 65535 || slices.Contains(a.t.ReservedPorts, p.Port) {
			return nil, invalidApp("port " + strconv.Itoa(p.Port) + " tidak bisa dipakai")
		}
		if !a.shares.p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer a.shares.p.write.Unlock()
		a.mu.Lock()
		defer a.mu.Unlock()
		st, err := a.load()
		if err != nil {
			return nil, err
		}
		for id, s := range st.Apps {
			if s.Installed && id != p.ID && s.Port == p.Port {
				return nil, &OpError{Code: proto.ErrConflict, Message: "port " + strconv.Itoa(p.Port) + " sudah dipakai " + id}
			}
		}
		if !a.portFree(p.Port) {
			return nil, &OpError{Code: proto.ErrConflict, Message: "port " + strconv.Itoa(p.Port) + " sudah dipakai program lain di NAS"}
		}
		s := st.Apps[p.ID]
		switch {
		case s != nil && s.Installed:
			return nil, &OpError{Code: proto.ErrConflict, Message: "aplikasi sudah terpasang"}
		case s != nil && s.HasData && s.Pool != p.Pool:
			return nil, &OpError{Code: proto.ErrConflict, Message: "data aplikasi ini masih ada di pool " + s.Pool + "; pasang di pool itu, atau hapus datanya dulu"}
		case s == nil:
			if st.NextID > a.t.IDMax {
				return nil, &OpError{Code: proto.ErrConflict, Message: "rentang ID akun aplikasi habis"}
			}
			// Dicatat sebelum akun dibuat (seperti akun pengguna, ADR 0006).
			s = &appState{UID: st.NextID, Pool: p.Pool}
			st.NextID++
			st.Apps[p.ID] = s
			if err := a.save(st); err != nil {
				return nil, err
			}
		}
		if err := a.ensureAccount(ctx, p.ID, s.UID); err != nil {
			return nil, err
		}
		path, err := a.ensureData(ctx, p.Pool, p.ID, s.UID)
		if err != nil {
			return nil, err
		}
		// Data dicatat segera, agar tetap terlihat (dan bisa dihapus) walau
		// langkah berikutnya gagal.
		s.HasData = true
		if err := a.save(st); err != nil {
			return nil, err
		}
		s.Port, s.Installed, s.Enabled, s.InstalledAt = p.Port, true, true, time.Now().UTC()
		if err := a.writeQuadlet(def, s, path); err != nil {
			return nil, err
		}
		if err := a.save(st); err != nil {
			return nil, err
		}
		if err := a.systemctl(ctx, []string{"daemon-reload"}); err != nil {
			return nil, err
		}
		// Image diunduh saat pertama dijalankan (bisa lama): tidak ditunggu.
		if err := a.systemctl(ctx, []string{"start", "--no-block"}, appUnit(p.ID)); err != nil {
			return nil, err
		}
		a.logInfo("aplikasi dipasang", "app", p.ID, "pool", p.Pool, "port", p.Port)
		return map[string]bool{"ok": true}, nil
	}
}

func (a *Apps) logInfo(msg string, args ...any) {
	if a.t.Log != nil {
		a.t.Log.Info(msg, args...)
	}
}

// ensureAccount membuat grup dan akun aplikasi (UID = GID) bila belum ada.
// Nama atau ID yang sudah dipakai akun lain ditolak.
func (a *Apps) ensureAccount(ctx context.Context, id string, uid int) error {
	name := appAccount(id)
	users, err := readPasswd(a.shares.t.PasswdFile)
	if err != nil {
		return err
	}
	groups, err := readGroup(a.shares.t.GroupFile)
	if err != nil {
		return err
	}
	for n, u := range users {
		if (n == name) != (u.UID == uid) {
			return &OpError{Code: proto.ErrConflict, Message: "akun " + name + " atau UID " + strconv.Itoa(uid) + " sudah dipakai akun lain"}
		}
	}
	for n, g := range groups {
		if (n == name) != (g.GID == uid) {
			return &OpError{Code: proto.ErrConflict, Message: "grup " + name + " atau GID " + strconv.Itoa(uid) + " sudah dipakai grup lain"}
		}
	}
	run := func(prog string, flags []string, operand string) error {
		res, err := runTool(ctx, a.t.Timeout, prog, flags, operand)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s: %s", filepath.Base(prog), firstLine(res.Stderr))
		}
		return nil
	}
	if _, ok := groups[name]; !ok {
		if err := run(a.t.Groupadd, []string{"--system", "--gid", strconv.Itoa(uid)}, name); err != nil {
			return err
		}
	}
	if _, ok := users[name]; !ok {
		if err := run(a.t.Useradd, []string{"--system", "--uid", strconv.Itoa(uid), "--gid", strconv.Itoa(uid),
			"--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", "--comment", "Aplikasi Sapo " + id}, name); err != nil {
			return err
		}
	}
	return nil
}

// ensureData membuat dataset <pool>/sapo-apps/<id> (penanda sapo:app),
// lalu memastikan dataset itu terpasang di tempatnya dan dimiliki akun
// aplikasi.
func (a *Apps) ensureData(ctx context.Context, pool, id string, uid int) (string, error) {
	sh := a.shares
	pools, err := sh.p.poolNames(ctx)
	if err != nil {
		return "", err
	}
	if !pools[pool] {
		return "", &OpError{Code: proto.ErrNotFound, Message: "pool " + pool + " tidak ditemukan"}
	}
	if props, err := sh.zfsProps(ctx, pool, "mountpoint"); err != nil || props["mountpoint"] != filepath.Join(a.t.MountBase, pool) {
		return "", &OpError{Code: proto.ErrConflict, Message: "pool " + pool + " tidak terpasang di tempat Sapo"}
	}
	parent := pool + "/sapo-apps"
	pprops, err := sh.zfsProps(ctx, parent, "sapo:app", sharePropName)
	if errors.Is(err, errNoDataset) {
		if err := a.zfs(ctx, []string{"create", "-o", "sapo:app=-", "-o", "devices=off", "-o", "setuid=off"}, parent); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else if pprops["sapo:app"] != "-" || (pprops[sharePropName] != "-" && pprops[sharePropName] != "") {
		// Dataset bernama sama yang bukan buatan App Center (misalnya
		// dibuat dari terminal): tidak dipakai.
		return "", &OpError{Code: proto.ErrConflict, Message: "dataset " + parent + " sudah ada dan bukan milik App Center"}
	}
	ds := parent + "/" + id
	want := filepath.Join(a.t.MountBase, pool, "sapo-apps", id)
	props, err := sh.zfsProps(ctx, ds, "mountpoint", "mounted", "sapo:app")
	if errors.Is(err, errNoDataset) {
		if err := a.zfs(ctx, []string{"create", "-o", "sapo:app=" + id, "-o", "devices=off", "-o", "setuid=off", "-o", "exec=on"}, ds); err != nil {
			return "", err
		}
		props, err = sh.zfsProps(ctx, ds, "mountpoint", "mounted", "sapo:app")
	}
	if err != nil {
		return "", err
	}
	if props["sapo:app"] != id || props["mountpoint"] != want || props["mounted"] != "yes" {
		return "", &OpError{Code: proto.ErrConflict, Message: "dataset " + ds + " bukan data aplikasi Sapo atau belum terpasang"}
	}
	if fi, err := os.Lstat(want); err != nil || !fi.IsDir() || !sh.isMount(want) {
		return "", fmt.Errorf("%s belum terpasang di %s", ds, want)
	}
	if err := a.chown(want, uid, uid); err != nil {
		return "", err
	}
	if err := os.Chmod(want, 0o700); err != nil {
		return "", err
	}
	return want, nil
}

func (a *Apps) zfs(ctx context.Context, flags []string, ds string) error {
	res, err := a.shares.p.st.run(ctx, a.shares.t.Zfs, flags, ds)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("zfs %s: %s", flags[0], firstLine(res.Stderr))
	}
	return nil
}

// renderQuadlet menyusun file .container dari templat tetap. Pembatasan
// keamanan di sini tidak bisa diubah dari katalog.
func renderQuadlet(d appDef, s *appState, dataPath string) []byte {
	var b bytes.Buffer
	b.WriteString("# Dibuat oleh sapod (Sapo, App Center). Jangan diubah manual.\n")
	fmt.Fprintf(&b, "[Unit]\nDescription=Aplikasi Sapo: %s\nAfter=network-online.target local-fs.target zfs-mount.service\nWants=network-online.target\nRequiresMountsFor=%s\n", d.ID, dataPath)
	// Aplikasi yang terus gagal (misalnya datanya hilang) berhenti dicoba.
	b.WriteString("StartLimitIntervalSec=600\nStartLimitBurst=5\n\n")
	b.WriteString("[Container]\n")
	fmt.Fprintf(&b, "ContainerName=%s\nImage=%s\n", appAccount(d.ID), d.Image)
	fmt.Fprintf(&b, "User=%d\nGroup=%d\n", s.UID, s.UID)
	fmt.Fprintf(&b, "PublishPort=%d:%d\n", s.Port, d.WebPort)
	fmt.Fprintf(&b, "Volume=%s:%s\n", dataPath, d.DataPath)
	keys := make([]string, 0, len(d.Env))
	for k := range d.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "Environment=%s=%s\n", k, d.Env[k])
	}
	b.WriteString("DropCapability=all\nNoNewPrivileges=true\n")
	// Port di bawah 1024 tidak bisa dibuka di dalam container (misalnya untuk
	// menyamar sebagai klien NFS istimewa).
	fmt.Fprintf(&b, "PodmanArgs=--memory=%dm --pids-limit=1024 --sysctl=net.ipv4.ip_unprivileged_port_start=1024\n\n", d.MemoryMB)
	b.WriteString("[Service]\nRestart=on-failure\nRestartSec=10s\n# Unduhan image pertama bisa lama.\nTimeoutStartSec=900\n")
	if s.Enabled {
		b.WriteString("\n[Install]\nWantedBy=multi-user.target\n")
	}
	return b.Bytes()
}

func (a *Apps) quadletPath(id string) string {
	return filepath.Join(a.t.QuadletDir, appAccount(id)+".container")
}

func (a *Apps) writeQuadlet(d appDef, s *appState, dataPath string) error {
	if err := os.MkdirAll(a.t.QuadletDir, 0o755); err != nil {
		return err
	}
	return writeRootFile(a.quadletPath(d.ID), renderQuadlet(d, s, dataPath))
}

// setEnabled menjalankan (start) atau menghentikan (stop) aplikasi, termasuk
// saat boot.
func (a *Apps) setEnabled(enable bool) Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.AppParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		cat, err := a.catalog()
		if err != nil {
			return nil, err
		}
		if !a.shares.p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer a.shares.p.write.Unlock()
		a.mu.Lock()
		defer a.mu.Unlock()
		st, err := a.load()
		if err != nil {
			return nil, err
		}
		s, ok := st.Apps[p.ID]
		if !ok || !s.Installed {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "aplikasi tidak terpasang"}
		}
		def, inCat := cat[p.ID]
		if !inCat {
			// Sudah tidak ada di katalog: hanya bisa dihentikan (dan dilepas,
			// data disimpan).
			if enable {
				return nil, &OpError{Code: proto.ErrNotFound, Message: "aplikasi sudah tidak ada di katalog"}
			}
			if err := a.systemctl(ctx, []string{"stop"}, appUnit(p.ID)); err != nil {
				return nil, err
			}
			_ = os.Remove(a.quadletPath(p.ID))
			s.Installed, s.Enabled = false, false
			if err := a.save(st); err != nil {
				return nil, err
			}
			_ = a.systemctl(ctx, []string{"daemon-reload"})
			return map[string]bool{"ok": true}, nil
		}
		path := filepath.Join(a.t.MountBase, s.Pool, "sapo-apps", p.ID)
		if enable {
			// Data diperiksa lagi (terpasang di tempatnya, pemilik akun).
			if path, err = a.ensureData(ctx, s.Pool, p.ID, s.UID); err != nil {
				return nil, err
			}
		}
		s.Enabled = enable
		if err := a.writeQuadlet(def, s, path); err != nil {
			return nil, err
		}
		if err := a.save(st); err != nil {
			return nil, err
		}
		if err := a.systemctl(ctx, []string{"daemon-reload"}); err != nil {
			return nil, err
		}
		action := []string{"start", "--no-block"}
		if !enable {
			action = []string{"stop"}
		}
		if err := a.systemctl(ctx, action, appUnit(p.ID)); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	}
}

// Reconcile menulis ulang file Quadlet aplikasi terpasang dari katalog saat
// sapod menyala (misalnya digest image baru setelah paket Sapo diperbarui),
// lalu me-restart aplikasi yang berubah.
func (a *Apps) Reconcile(ctx context.Context) {
	cat, err := a.catalog()
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.load()
	if err != nil {
		return
	}
	var changed []string
	for id, s := range st.Apps {
		def, ok := cat[id]
		if !s.Installed || !ok {
			continue
		}
		want := renderQuadlet(def, s, filepath.Join(a.t.MountBase, s.Pool, "sapo-apps", id))
		if cur, err := os.ReadFile(a.quadletPath(id)); err == nil && bytes.Equal(cur, want) {
			continue
		}
		if err := a.writeQuadlet(def, s, filepath.Join(a.t.MountBase, s.Pool, "sapo-apps", id)); err == nil {
			changed = append(changed, id)
		}
	}
	if len(changed) == 0 {
		return
	}
	_ = a.systemctl(ctx, []string{"daemon-reload"})
	for _, id := range changed {
		_ = a.systemctl(ctx, []string{"try-restart"}, appUnit(id))
		a.logInfo("aplikasi diperbarui dari katalog", "app", id)
	}
}

// StartOp dan StopOp mengembalikan handler apps.start dan apps.stop.
func (a *Apps) StartOp() Handler { return a.setEnabled(true) }
func (a *Apps) StopOp() Handler  { return a.setEnabled(false) }

// RemoveOp mengembalikan handler apps.remove: aplikasi dihentikan dan
// dilepas; data (dataset dan akun) hanya dihapus bila RemoveData.
func (a *Apps) RemoveOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.AppRemoveParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !appIDPattern.MatchString(p.ID) {
			return nil, invalidApp("aplikasi tidak valid")
		}
		if !a.shares.p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer a.shares.p.write.Unlock()
		a.mu.Lock()
		defer a.mu.Unlock()
		st, err := a.load()
		if err != nil {
			return nil, err
		}
		s, ok := st.Apps[p.ID]
		if !ok || (!s.Installed && !s.HasData) {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "aplikasi tidak ditemukan"}
		}
		if s.Installed {
			// Bila tidak bisa dihentikan, tidak ada yang diubah (container
			// tidak boleh tertinggal berjalan tanpa unit).
			if err := a.systemctl(ctx, []string{"stop"}, appUnit(p.ID)); err != nil {
				return nil, err
			}
			if err := os.Remove(a.quadletPath(p.ID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			_ = a.systemctl(ctx, []string{"daemon-reload"})
			s.Installed, s.Enabled = false, false
			if err := a.save(st); err != nil {
				return nil, err
			}
		}
		if p.RemoveData && s.HasData {
			ds := s.Pool + "/sapo-apps/" + p.ID
			props, err := a.shares.zfsProps(ctx, ds, "sapo:app")
			switch {
			case errors.Is(err, errNoDataset):
			case err != nil:
				return nil, err
			case props["sapo:app"] != p.ID:
				return nil, &OpError{Code: proto.ErrConflict, Message: "dataset " + ds + " bukan data aplikasi Sapo"}
			default:
				if err := a.zfs(ctx, []string{"destroy", "-r"}, ds); err != nil {
					return nil, &OpError{Code: proto.ErrPoolBusy, Message: err.Error()}
				}
			}
			s.HasData = false
			if err := a.save(st); err != nil {
				return nil, err
			}
			// Akun dihapus bersama datanya (ID tidak pernah dipakai ulang).
			// Bila gagal, catatannya disimpan agar dicoba lagi.
			name := appAccount(p.ID)
			users, _ := readPasswd(a.shares.t.PasswdFile)
			if _, exists := users[name]; exists {
				if res, err := runTool(ctx, a.t.Timeout, a.t.Userdel, nil, name); err != nil || res.ExitCode != 0 {
					return nil, fmt.Errorf("akun %s gagal dihapus: %v %s", name, err, firstLine(res.Stderr))
				}
			}
			groups, _ := readGroup(a.shares.t.GroupFile)
			if _, exists := groups[name]; exists {
				if res, err := runTool(ctx, a.t.Timeout, a.t.Groupdel, nil, name); err != nil || res.ExitCode != 0 {
					return nil, fmt.Errorf("grup %s gagal dihapus: %v %s", name, err, firstLine(res.Stderr))
				}
			}
			delete(st.Apps, p.ID)
			if err := a.save(st); err != nil {
				return nil, err
			}
		}
		a.logInfo("aplikasi dihapus", "app", p.ID, "data_dihapus", p.RemoveData)
		return map[string]bool{"ok": true}, nil
	}
}
