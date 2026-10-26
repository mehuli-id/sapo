package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Backup ke S3/cloud (ADR 0020) dengan restic. Data dienkripsi di NAS
// (password repo); penyedia cloud hanya melihat data acak.
//
// sapod sendiri tidak pernah ke jaringan: restic dijalankan lewat
// systemd-run di unit sementara yang dikunci (sistem file baca saja kecuali
// cache dan tujuan pemulihan, kapabilitas seminimal mungkin). Password repo
// dan kunci S3 hanya disimpan sapod (secrets_dir, 0600) dan masuk ke unit
// lewat LoadCredential, bukan variabel lingkungan (yang bisa dibaca siapa
// pun lewat systemctl show). Tidak ada rahasia yang dikembalikan ke panel,
// kecuali password repo baru sekali saat dibuat.

var (
	cloudHostPattern   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	cloudBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	cloudPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+){0,7}$`)
	cloudRegionPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	cloudKeyPattern    = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
	cloudNamePattern   = regexp.MustCompile(`^[\p{L}\p{N} ._-]{1,40}$`)
	resticIDPattern    = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
)

// CloudTools mengatur backup cloud (agent.cloud di sapo.yaml).
type CloudTools struct {
	Restic, SystemdRun, Systemctl, Timedatectl, Journalctl string
	StateFile, SecretsDir                                  string
	CacheDir                                               string
	MaxRun                                                 time.Duration
	// ListMax: batas isi satu folder saat menelusuri salinan.
	ListMax int
}

// Cloud melayani cloud.* dan menjalankan jadwalnya.
type Cloud struct {
	t       CloudTools
	b       *Backups
	mu      sync.Mutex // state file
	runMu   sync.Mutex // satu restic pada satu waktu
	running map[string]bool
	// exec menjalankan perintah lengkap (systemd-run …) dan mengembalikan
	// stdout; diganti di test.
	exec func(ctx context.Context, unit string, argv []string) ([]byte, error)
	// clockOK: jam NAS bisa dipercaya untuk memangkas salinan (diganti di
	// test).
	clockOK func(ctx context.Context) bool
	// snapCache: daftar salinan per repo untuk menelusuri isi (setiap klik
	// folder tidak perlu meminta daftar salinan lagi ke S3).
	cacheMu   sync.Mutex
	snapCache map[string]cachedSnaps
}

type cachedSnaps struct {
	at   time.Time
	list []proto.CloudSnapshot
}

const snapCacheTTL = 2 * time.Minute

type cloudJobState struct {
	proto.CloudJob
	LastRun     time.Time `json:"last_run,omitempty"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastOK      bool      `json:"last_ok"`
	LastError   string    `json:"last_error,omitempty"`
	LastAdded   uint64    `json:"last_added,omitempty"`
	LastTotal   uint64    `json:"last_total,omitempty"`
}

type cloudState struct {
	Repos map[string]*proto.CloudRepo `json:"repos"`
	Jobs  map[string]*cloudJobState   `json:"jobs"`
}

// NewCloud menyiapkan backup cloud di atas b (berbagi kunci pool, catatan
// pemulihan, dan snapshot).
func NewCloud(t CloudTools, b *Backups) *Cloud {
	if t.MaxRun == 0 {
		t.MaxRun = 24 * time.Hour
	}
	if t.ListMax == 0 {
		t.ListMax = 5000
	}
	c := &Cloud{t: t, b: b, running: map[string]bool{}, snapCache: map[string]cachedSnaps{}}
	c.exec = c.systemdExec
	c.clockOK = c.ntpOK
	b.shares.cloud = c
	b.shares.p.beforeDestroy = append(b.shares.p.beforeDestroy, c.refusePool)
	return c
}

func (c *Cloud) load() (cloudState, error) {
	st := cloudState{Repos: map[string]*proto.CloudRepo{}, Jobs: map[string]*cloudJobState{}}
	data, err := os.ReadFile(c.t.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("catatan backup cloud rusak (%s): %w", c.t.StateFile, err)
	}
	if st.Repos == nil {
		st.Repos = map[string]*proto.CloudRepo{}
	}
	if st.Jobs == nil {
		st.Jobs = map[string]*cloudJobState{}
	}
	return st, nil
}

func (c *Cloud) get() (cloudState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.load()
}

func (c *Cloud) update(fn func(*cloudState) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, err := c.load()
	if err != nil {
		return err
	}
	if err := fn(&st); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(c.t.StateFile, data)
}

func (c *Cloud) available() bool {
	st, err := os.Stat(c.t.Restic)
	return err == nil && !st.IsDir()
}

func (c *Cloud) usingShare(pool, share string) string {
	st, err := c.get()
	if err != nil {
		return "?"
	}
	for _, j := range st.Jobs {
		if j.Pool == pool && j.Share == share {
			return j.ID
		}
	}
	return ""
}

func (c *Cloud) refusePool(_ context.Context, pool string) error {
	st, err := c.get()
	if err != nil {
		return err
	}
	for _, j := range st.Jobs {
		if j.Pool == pool {
			return &OpError{Code: proto.ErrConflict, Message: "pool " + pool + " masih dipakai backup cloud " + j.ID + "; hapus tugas itu dulu"}
		}
	}
	return nil
}

func (c *Cloud) pwFile(id string) string { return filepath.Join(c.t.SecretsDir, id+".pw") }
func (c *Cloud) s3File(id string) string { return filepath.Join(c.t.SecretsDir, id+".s3") }

func repoURL(r proto.CloudRepo) string {
	scheme := "https"
	if r.InsecureHTTP {
		scheme = "http"
	}
	u := "s3:" + scheme + "://" + r.Endpoint + "/" + r.Bucket
	if r.Prefix != "" {
		u += "/" + r.Prefix
	}
	return u
}

func validRepo(p proto.CloudRepoParams) error {
	switch {
	case !cloudNamePattern.MatchString(p.Name):
		return errors.New("nama tidak valid")
	case !cloudHostPattern.MatchString(p.Endpoint):
		return errors.New("alamat server S3 tidak valid")
	case !cloudBucketPattern.MatchString(p.Bucket):
		return errors.New("nama bucket tidak valid")
	case p.Prefix != "" && (!cloudPrefixPattern.MatchString(p.Prefix) || slices.Contains(strings.Split(p.Prefix, "/"), "..")):
		return errors.New("folder di bucket tidak valid")
	case p.Region != "" && !cloudRegionPattern.MatchString(p.Region):
		return errors.New("region tidak valid")
	case !cloudKeyPattern.MatchString(p.AccessKey) || !cloudKeyPattern.MatchString(p.SecretKey):
		return errors.New("kunci akses tidak valid")
	case p.Existing && (len(p.Password) < 8 || len(p.Password) > 256 || strings.ContainsAny(p.Password, "\r\n\x00")):
		return errors.New("password repo tidak valid")
	}
	return nil
}

// newRepoPassword: 160 bit acak, dikelompokkan agar mudah dicatat.
func newRepoPassword() string {
	raw := make([]byte, 20)
	_, _ = rand.Read(raw)
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	var parts []string
	for i := 0; i < len(s); i += 4 {
		parts = append(parts, s[i:min(i+4, len(s))])
	}
	return strings.Join(parts, "-")
}

// resticMode: hak unit restic. restic berjalan sebagai pengguna dinamis
// (DynamicUser, bukan root); caps diberikan sebagai ambient capability.
type resticMode struct {
	caps     string   // kapabilitas (kosong = tanpa kapabilitas)
	writable []string // ReadWritePaths tambahan selain cache
}

var (
	modeRead   = resticMode{}
	modeBackup = resticMode{caps: "CAP_DAC_READ_SEARCH"}
	// Pemulihan menulis file milik pengguna lain (pemilik di-reset setelahnya).
	modeRestore = func(target string) resticMode {
		return resticMode{caps: "CAP_CHOWN CAP_FOWNER CAP_DAC_OVERRIDE", writable: []string{target}}
	}
)

// command menyusun systemd-run untuk satu perintah restic.
func (c *Cloud) command(r proto.CloudRepo, mode resticMode, args ...string) (string, []string) {
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	unit := "sapo-cloud-" + hex.EncodeToString(raw)
	creds := "/run/credentials/" + unit + ".service/"

	// Tidak terlihat sama sekali dari unit: rahasia, data panel, dan socket
	// sapod/D-Bus. /run/systemd tidak bisa disembunyikan (systemd memakai
	// /run/systemd/inaccessible untuk InaccessiblePaths); socket systemd
	// tetap tak terjangkau karena unit tanpa AF_UNIX.
	hidden := strings.Join([]string{c.t.SecretsDir, filepath.Dir(c.t.StateFile), "/var/lib/sapo", "/etc/sapo", "/etc/shadow", "/etc/gshadow",
		"/run/sapo", "/run/dbus", "/var/lib/samba"}, " ")
	argv := []string{c.t.SystemdRun,
		"--unit=" + unit, "--description=Backup cloud Sapo", "--collect", "--quiet", "--wait", "--pipe",
		"--property=Type=exec",
		fmt.Sprintf("--property=RuntimeMaxSec=%d", int(c.t.MaxRun.Seconds())),
		"--property=DynamicUser=yes", "--property=CacheDirectory=" + filepath.Base(c.t.CacheDir), "--property=CacheDirectoryMode=0700",
		"--property=InaccessiblePaths=-" + strings.ReplaceAll(hidden, " ", " -"),
		"--property=NoNewPrivileges=yes", "--property=ProtectSystem=strict", "--property=ProtectHome=yes",
		"--property=PrivateTmp=yes", "--property=PrivateDevices=yes", "--property=PrivateIPC=yes",
		"--property=ProtectKernelTunables=yes", "--property=ProtectKernelModules=yes", "--property=ProtectKernelLogs=yes",
		"--property=ProtectControlGroups=yes", "--property=ProtectClock=yes", "--property=ProtectHostname=yes",
		"--property=RestrictNamespaces=yes", "--property=RestrictSUIDSGID=yes", "--property=RestrictRealtime=yes",
		"--property=LockPersonality=yes", "--property=MemoryDenyWriteExecute=yes", "--property=SystemCallArchitectures=native",
		// Tanpa socket unix: pengguna dengan CAP_DAC_OVERRIDE (pemulihan) bisa
		// menyambung ke socket mana pun.
		"--property=RestrictAddressFamilies=AF_INET AF_INET6",
		"--property=CapabilityBoundingSet=" + mode.caps,
		"--property=AmbientCapabilities=" + mode.caps,
		// Resolver Go murni (tanpa nss/socket lokal).
		"--setenv=GODEBUG=netdns=go",
		// Cache restic: CacheDirectory (milik pengguna dinamis), bukan
		// ReadWritePaths.
		"--property=LoadCredential=restic-pw:" + c.pwFile(r.ID),
		"--property=LoadCredential=s3:" + c.s3File(r.ID),
		"--setenv=AWS_SHARED_CREDENTIALS_FILE=" + creds + "s3",
		"--setenv=RESTIC_CACHE_DIR=" + c.t.CacheDir,
		"--setenv=HOME=" + c.t.CacheDir,
	}
	if len(mode.writable) > 0 {
		argv = append(argv, "--property=ReadWritePaths="+strings.Join(mode.writable, " "))
	}
	argv = append(argv, "--", c.t.Restic, "--repo", repoURL(r), "--password-file", creds+"restic-pw")
	if r.Region != "" {
		argv = append(argv, "-o", "s3.region="+r.Region)
	}
	return unit, append(argv, args...)
}

// tailBuffer menyimpan max byte TERAKHIR (ringkasan restic ada di akhir).
type tailBuffer struct {
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (c *Cloud) systemdExec(ctx context.Context, unit string, argv []string) ([]byte, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = toolEnv
	// Daftar salinan bisa besar setelah bertahun-tahun (ratusan salinan per
	// folder): buffer cukup besar agar JSON tidak terpotong.
	out, errb := &tailBuffer{max: 64 << 20}, &tailBuffer{max: 8 << 10}
	cmd.Stdout, cmd.Stderr = out, errb
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Dibatalkan: unit dihentikan (systemd-run --wait sendiri tidak).
	stop := context.AfterFunc(ctx, func() {
		_, _ = runTool(context.Background(), 30*time.Second, c.t.Systemctl, []string{"stop"}, unit+".service")
	})
	defer stop()
	err := cmd.Wait()
	if err != nil {
		if msg := lastLine(errb.b); msg != "" {
			return out.b, errors.New(msg)
		}
		// Unit gagal sebelum restic berjalan (misalnya sandbox): alasan dari
		// journal unit.
		if c.t.Journalctl != "" {
			if res, jerr := runTool(context.Background(), 15*time.Second, c.t.Journalctl, []string{"-u", unit + ".service", "-n", "5", "-o", "cat", "--no-pager"}); jerr == nil {
				if msg := lastLine(res.Stdout); msg != "" {
					return out.b, fmt.Errorf("restic: %w (%s)", err, msg)
				}
			}
		}
		return out.b, fmt.Errorf("restic: %w", err)
	}
	return out.b, nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return clipTo(l, maxErrorText)
		}
	}
	return ""
}

func (c *Cloud) restic(ctx context.Context, r proto.CloudRepo, mode resticMode, args ...string) ([]byte, error) {
	unit, argv := c.command(r, mode, args...)
	return c.exec(ctx, unit, argv)
}

// resticError: galat restic yang dikenal → kode untuk panel.
func resticError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "wrong password") || strings.Contains(msg, "no key found"):
		return &OpError{Code: "cloud_bad_password", Message: "password repo salah"}
	case strings.Contains(msg, "already exists") || strings.Contains(msg, "already initialized"):
		return &OpError{Code: "cloud_repo_exists", Message: "sudah ada repo di tempat ini; sambungkan dengan password-nya"}
	case strings.Contains(msg, "Access Denied") || strings.Contains(msg, "InvalidAccessKeyId") || strings.Contains(msg, "SignatureDoesNotMatch"):
		return &OpError{Code: "cloud_denied", Message: "server S3 menolak kunci akses: " + clipTo(msg, maxErrorText)}
	}
	return &OpError{Code: "cloud_failed", Message: clipTo(msg, maxErrorText)}
}

// --- Handler -----------------------------------------------------------------

// ListOp: cloud.list.
func (c *Cloud) ListOp() Handler {
	return func(context.Context, json.RawMessage) (any, error) {
		st, err := c.get()
		if err != nil {
			return nil, err
		}
		out := proto.CloudListResult{Available: c.available(), Repos: []proto.CloudRepo{}, Jobs: []proto.CloudJobStatus{}}
		for _, r := range st.Repos {
			out.Repos = append(out.Repos, *r)
		}
		sort.Slice(out.Repos, func(i, j int) bool { return out.Repos[i].Created.Before(out.Repos[j].Created) })
		c.mu.Lock()
		running := maps(c.running)
		c.mu.Unlock()
		for _, j := range st.Jobs {
			out.Jobs = append(out.Jobs, proto.CloudJobStatus{CloudJob: j.CloudJob, Running: running[j.ID], LastRun: j.LastRun,
				LastOK: j.LastOK, LastError: j.LastError, LastAdded: j.LastAdded, LastTotal: j.LastTotal})
		}
		sort.Slice(out.Jobs, func(i, j int) bool { return out.Jobs[i].ID < out.Jobs[j].ID })
		return out, nil
	}
}

// RepoAddOp: cloud.repo_add. Repo baru: restic init dengan password acak
// (dikembalikan sekali). Repo yang ada: password diperiksa (cat config).
func (c *Cloud) RepoAddOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.CloudRepoParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := validRepo(p); err != nil {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		if !c.available() {
			return nil, &OpError{Code: proto.ErrUnavailable, Message: "restic belum terpasang (aktifkan fitur Backup cloud)"}
		}
		idRaw := make([]byte, 6)
		_, _ = rand.Read(idRaw)
		r := proto.CloudRepo{ID: hex.EncodeToString(idRaw), Name: p.Name, Endpoint: p.Endpoint, Bucket: p.Bucket, Prefix: p.Prefix,
			Region: p.Region, InsecureHTTP: p.InsecureHTTP, Created: c.b.now().UTC()}
		pw := p.Password
		if !p.Existing {
			pw = newRepoPassword()
		}
		if err := os.MkdirAll(c.t.SecretsDir, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(c.t.SecretsDir, 0o700); err != nil {
			return nil, err
		}
		s3 := "[default]\naws_access_key_id = " + p.AccessKey + "\naws_secret_access_key = " + p.SecretKey + "\n"
		if err := writePrivateFile(c.pwFile(r.ID), []byte(pw)); err != nil {
			return nil, err
		}
		if err := writePrivateFile(c.s3File(r.ID), []byte(s3)); err != nil {
			os.Remove(c.pwFile(r.ID))
			return nil, err
		}
		cleanup := func() { os.Remove(c.pwFile(r.ID)); os.Remove(c.s3File(r.ID)) }
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		args := []string{"init"}
		if p.Existing {
			args = []string{"cat", "config"}
		}
		if _, err := c.restic(ctx, r, modeRead, args...); err != nil {
			cleanup()
			return nil, resticError(err)
		}
		if err := c.update(func(st *cloudState) error { st.Repos[r.ID] = &r; return nil }); err != nil {
			cleanup()
			return nil, err
		}
		c.b.shares.p.logInfo("repo backup cloud ditambahkan", "repo", r.ID, "server", r.Endpoint, "bucket", r.Bucket, "sudah_ada", p.Existing)
		out := proto.CloudRepoAdded{ID: r.ID}
		if !p.Existing {
			out.Password = pw
		}
		return out, nil
	}
}

// RepoDeleteOp: cloud.repo_delete. Data di cloud TIDAK dihapus; NAS ini
// hanya melupakan repo dan rahasianya.
func (c *Cloud) RepoDeleteOp() Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var p proto.CloudIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		err := c.update(func(st *cloudState) error {
			if _, ok := st.Repos[p.ID]; !ok {
				return &OpError{Code: proto.ErrNotFound, Message: "repo tidak ditemukan"}
			}
			for _, j := range st.Jobs {
				if j.Repo == p.ID {
					return &OpError{Code: proto.ErrConflict, Message: "repo masih dipakai tugas " + j.ID}
				}
			}
			delete(st.Repos, p.ID)
			return nil
		})
		if err != nil {
			return nil, err
		}
		os.Remove(c.pwFile(p.ID))
		os.Remove(c.s3File(p.ID))
		return map[string]bool{"ok": true}, nil
	}
}

type resticSnapshot struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags"`
}

func (c *Cloud) listSnapshots(ctx context.Context, r proto.CloudRepo) ([]proto.CloudSnapshot, error) {
	out, err := c.restic(ctx, r, modeRead, "snapshots", "--json", "--tag", "sapo")
	if err != nil {
		return nil, resticError(err)
	}
	var raw []resticSnapshot
	if err := json.Unmarshal(bytes.TrimSpace(out), &raw); err != nil {
		return nil, &OpError{Code: "cloud_failed", Message: "daftar salinan restic tidak terbaca"}
	}
	list := []proto.CloudSnapshot{}
	for _, s := range raw {
		if !resticIDPattern.MatchString(s.ID) || len(s.Paths) != 1 || !filepath.IsAbs(s.Paths[0]) {
			continue
		}
		cs := proto.CloudSnapshot{ID: s.ID, Time: s.Time.UTC(), Hostname: clipTo(s.Hostname, 64), Path: s.Paths[0]}
		for _, t := range s.Tags {
			if v, ok := strings.CutPrefix(t, "share:"); ok {
				cs.Share = clipTo(v, 64)
			}
			if v, ok := strings.CutPrefix(t, "job:"); ok {
				cs.Job = clipTo(v, 64)
			}
		}
		list = append(list, cs)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Time.Before(list[j].Time) })
	return list, nil
}

// SnapshotsOp: cloud.snapshots (semua salinan Sapo di repo, juga dari NAS
// lama: untuk pemulihan setelah NAS diganti).
func (c *Cloud) SnapshotsOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.CloudIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		st, err := c.get()
		if err != nil {
			return nil, err
		}
		r, ok := st.Repos[p.ID]
		if !ok {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "repo tidak ditemukan"}
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		return c.listSnapshots(ctx, *r)
	}
}

// JobSetOp: cloud.job_set. Folder dan repo tugas yang sudah ada tidak
// berubah; hanya jadwal, lama simpan, dan nyala/mati.
func (c *Cloud) JobSetOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var j proto.CloudJob
		if err := decodeParams(raw, &j); err != nil {
			return nil, err
		}
		if j.ID == "" {
			j.ID = j.Share + "-c" + j.Repo[:min(len(j.Repo), 6)]
		}
		switch {
		case !backupIDPattern.MatchString(j.ID) || !poolNamePattern.MatchString(j.Pool) || !shareNamePattern.MatchString(j.Share):
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "tugas tidak valid"}
		case j.Hour < 0 || j.Hour > 23 || j.KeepDays < 1 || j.KeepDays > 3660:
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "jam 0–23 dan lama simpan 1–3660 hari"}
		}
		if err := c.b.checkSource(ctx, proto.BackupJob{SrcPool: j.Pool, Share: j.Share}); err != nil {
			return nil, err
		}
		err := c.update(func(st *cloudState) error {
			if _, ok := st.Repos[j.Repo]; !ok {
				return &OpError{Code: proto.ErrNotFound, Message: "repo tidak ditemukan"}
			}
			if cur, ok := st.Jobs[j.ID]; ok {
				if cur.Repo != j.Repo || cur.Pool != j.Pool || cur.Share != j.Share {
					return &OpError{Code: proto.ErrConflict, Message: "folder dan repo tugas yang sudah ada tidak bisa diubah"}
				}
				cur.Hour, cur.KeepDays, cur.Enabled = j.Hour, j.KeepDays, j.Enabled
				return nil
			}
			st.Jobs[j.ID] = &cloudJobState{CloudJob: j}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": j.ID}, nil
	}
}

func cloudSnapName(job string) string { return "sapo-cloud-" + job }

// JobDeleteOp: cloud.job_delete. Salinan di cloud TIDAK dihapus (bisa
// dipulihkan nanti dengan menyambung repo); snapshot sementara di folder
// dibuang.
func (c *Cloud) JobDeleteOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.CloudIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		c.mu.Lock()
		busy := c.running[p.ID]
		c.mu.Unlock()
		if busy {
			return nil, &OpError{Code: proto.ErrBusy, Message: "tugas sedang berjalan"}
		}
		var job proto.CloudJob
		err := c.update(func(st *cloudState) error {
			j, ok := st.Jobs[p.ID]
			if !ok {
				return &OpError{Code: proto.ErrNotFound, Message: "tugas tidak ditemukan"}
			}
			job = j.CloudJob
			delete(st.Jobs, p.ID)
			return nil
		})
		if err != nil {
			return nil, err
		}
		snap := job.Pool + "/" + job.Share + "@" + cloudSnapName(job.ID)
		if _, err := c.b.propsFn(ctx, snap, "type"); err == nil {
			c.b.discardSnap(ctx, snap)
		}
		return map[string]bool{"ok": true}, nil
	}
}

// RunOp: cloud.run (backup sekarang, di latar belakang).
func (c *Cloud) RunOp() Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var p proto.CloudIDParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		st, err := c.get()
		if err != nil {
			return nil, err
		}
		if _, ok := st.Jobs[p.ID]; !ok {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "tugas tidak ditemukan"}
		}
		if !c.runMu.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "backup cloud lain sedang berjalan"}
		}
		c.runMu.Unlock()
		go func() { _ = c.execute(context.Background(), p.ID) }()
		return map[string]bool{"ok": true}, nil
	}
}

// execute: satu backup. Snapshot ZFS bernama tetap (path sama setiap kali,
// sehingga restic hanya membaca file yang berubah) dibuat, dicadangkan,
// lalu dibuang; salinan di cloud lebih tua dari KeepDays dipangkas (yang
// terbaru selalu tetap).
func (c *Cloud) execute(ctx context.Context, id string) error {
	if !c.runMu.TryLock() {
		return &OpError{Code: proto.ErrBusy, Message: "backup cloud lain sedang berjalan"}
	}
	defer c.runMu.Unlock()
	st, err := c.get()
	if err != nil {
		return err
	}
	js, ok := st.Jobs[id]
	if !ok {
		return &OpError{Code: proto.ErrNotFound, Message: "tugas tidak ditemukan"}
	}
	repo, ok := st.Repos[js.Repo]
	if !ok {
		return &OpError{Code: proto.ErrNotFound, Message: "repo tugas ini tidak ada lagi"}
	}
	c.mu.Lock()
	c.running[id] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.running, id)
		c.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, c.t.MaxRun)
	defer cancel()
	start := c.b.now().UTC()
	added, total, runErr := c.backup(ctx, js.CloudJob, *repo)
	err = c.update(func(st *cloudState) error {
		j, ok := st.Jobs[id]
		if !ok {
			return nil
		}
		j.LastAttempt, j.LastOK, j.LastError = start, runErr == nil, ""
		if runErr != nil {
			j.LastError = clipTo(runErr.Error(), maxErrorText)
		} else {
			j.LastRun, j.LastAdded, j.LastTotal = start, added, total
		}
		return nil
	})
	if runErr != nil {
		c.b.shares.p.logWarn("backup cloud gagal", "tugas", id, "err", runErr)
		return runErr
	}
	c.b.shares.p.logInfo("backup cloud selesai", "tugas", id, "byte_baru", added)
	return err
}

func (c *Cloud) backup(ctx context.Context, j proto.CloudJob, r proto.CloudRepo) (uint64, uint64, error) {
	if err := c.b.checkSource(ctx, proto.BackupJob{SrcPool: j.Pool, Share: j.Share}); err != nil {
		return 0, 0, err
	}
	ds := j.Pool + "/" + j.Share
	snap := ds + "@" + cloudSnapName(j.ID)
	if err := c.b.locked(ctx, func() error {
		if _, err := c.b.propsFn(ctx, snap, "type"); err == nil {
			if _, err := c.b.zfs(ctx, []string{"destroy"}, snap); err != nil {
				return err
			}
		}
		_, err := c.b.zfs(ctx, []string{"snapshot", "-o", snapAutoProp + "=backup"}, snap)
		return err
	}); err != nil {
		return 0, 0, err
	}
	defer c.b.discardSnap(ctx, snap)
	path := filepath.Join(c.b.shares.t.MountBase, j.Pool, j.Share, ".zfs", "snapshot", cloudSnapName(j.ID))
	// Dipasang (automount) di namespace sapod sebelum unit restic dimulai,
	// karena unit itu memakai namespace mount sendiri.
	if _, err := os.ReadDir(path); err != nil {
		return 0, 0, fmt.Errorf("snapshot tidak bisa dibuka: %w", err)
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "sapo"
	}
	out, err := c.restic(ctx, r, modeBackup, "backup", "--json", "--exclude-caches",
		"--host", host, "--tag", "sapo", "--tag", "job:"+j.ID, "--tag", "share:"+j.Share, "--", path)
	if err != nil {
		return 0, 0, resticError(err)
	}
	var added, total uint64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var m struct {
			Type  string `json:"message_type"`
			Added uint64 `json:"data_added"`
			Total uint64 `json:"total_bytes_processed"`
		}
		if json.Unmarshal([]byte(line), &m) == nil && m.Type == "summary" {
			added, total = m.Added, m.Total
		}
	}
	// Pemangkasan hanya bila jam NAS bisa dipercaya: --keep-within dihitung
	// dari salinan terbaru, jadi salinan bertanggal masa depan (jam yang
	// salah) bisa membuat semua salinan asli terbuang. Per NAS (host) dan
	// tugas; minimal 7 salinan harian selalu tersisa.
	if !c.clockOK(ctx) {
		c.b.shares.p.logWarn("jam NAS belum tersinkron; pemangkasan backup cloud ditunda", "tugas", j.ID)
	} else if _, err := c.restic(ctx, r, modeRead, "forget", "--host", host, "--tag", "job:"+j.ID, "--group-by", "host,tags",
		"--keep-within", strconv.Itoa(j.KeepDays)+"d", "--keep-daily", "7", "--keep-last", "1", "--prune"); err != nil {
		// Backup sudah aman; pemangkasan dicoba lagi di backup berikutnya.
		c.b.shares.p.logWarn("pemangkasan backup cloud gagal", "tugas", j.ID, "err", err)
	}
	return added, total, nil
}

// due: sama dengan jadwal backup harian (ADR 0017).
func (c *Cloud) due(j *cloudJobState, now time.Time) bool {
	if !j.Enabled {
		return false
	}
	start := periodStart("daily", proto.SnapshotPolicy{Hour: j.Hour}, now, c.b.loc())
	if !j.LastRun.Before(start) {
		return false
	}
	return j.LastAttempt.IsZero() || now.Sub(j.LastAttempt) >= time.Hour || j.LastAttempt.Before(start)
}

// Run memeriksa jadwal setiap menit.
func (c *Cloud) Run(ctx context.Context) {
	for {
		if st, err := c.get(); err == nil && c.available() {
			ids := make([]string, 0, len(st.Jobs))
			for id := range st.Jobs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				if c.due(st.Jobs[id], c.b.now()) {
					_ = c.execute(ctx, id)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// RestoreOp: cloud.restore. Salinan dipulihkan ke shared folder BARU (dataset
// baru dibuat sapod; bila gagal, dataset itu dibuang lagi). Unit restic
// hanya boleh menulis ke folder baru itu.
func (c *Cloud) RestoreOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.CloudRestoreParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		switch {
		case !resticIDPattern.MatchString(p.Snapshot):
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "salinan tidak valid"}
		case !poolNamePattern.MatchString(p.Pool):
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "pool tidak valid"}
		case !shareNamePattern.MatchString(p.Name) || slices.Contains(reservedShares, p.Name):
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama folder tidak valid"}
		}
		rel, err := cloudRelPath(p.Path)
		if err != nil {
			return nil, err
		}
		st, err := c.get()
		if err != nil {
			return nil, err
		}
		r, ok := st.Repos[p.Repo]
		if !ok {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "repo tidak ditemukan"}
		}
		pools, err := c.b.poolsFn(ctx)
		if err != nil {
			return nil, err
		}
		if !pools[p.Pool] {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "pool tidak ditemukan"}
		}
		lctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		snaps, err := c.listSnapshots(lctx, *r)
		cancel()
		if err != nil {
			return nil, err
		}
		snap, err := pickSnapshot(snaps, p.Snapshot)
		if err != nil {
			return nil, err
		}
		mp := filepath.Join(c.b.shares.t.MountBase, p.Pool, p.Name)
		args := []string{"restore", snap.ID + ":" + snap.Path, "--target", mp}
		if rel != "" {
			// Hanya file/folder itu (beserta isinya); folder induknya ikut
			// dibuat sehingga letaknya sama seperti di folder asal.
			args = append(args, "--include", "/"+resticPattern(rel))
		}
		target := p.Pool + "/" + p.Name
		repo := *r
		return map[string]bool{"ok": true}, c.b.runRestore(ctx, "cloud:"+repo.ID, snap.ID[:min(len(snap.ID), 16)], p.Pool, p.Name, func(ctx context.Context) error {
			if _, err := c.b.zfs(ctx, []string{"create", "-o", sharePropName + "=" + p.Name, "-o", "devices=off", "-o", "setuid=off"}, target); err != nil {
				return err
			}
			if props, err := c.b.propsFn(ctx, target, "mounted", "mountpoint"); err != nil || props["mounted"] != "yes" || props["mountpoint"] != mp {
				_, _ = c.b.zfs(context.WithoutCancel(ctx), []string{"destroy", "-r"}, target)
				return errors.New("folder baru tidak terpasang di " + mp)
			}
			if _, err := c.restic(ctx, repo, modeRestore(mp), args...); err != nil {
				// Folder baru buatan pemulihan ini sendiri: aman dibuang.
				_, _ = c.b.zfs(context.WithoutCancel(ctx), []string{"destroy", "-r"}, target)
				return resticError(err)
			}
			// restic tidak menganggap --include tanpa hasil sebagai galat.
			if rel != "" {
				if _, err := os.Lstat(filepath.Join(mp, rel)); err != nil {
					_, _ = c.b.zfs(context.WithoutCancel(ctx), []string{"destroy", "-r"}, target)
					return &OpError{Code: proto.ErrNotFound, Message: "file atau folder tidak ada di salinan"}
				}
			}
			// UID dan ACL dari repo bisa milik orang lain di NAS ini.
			if err := c.b.resetFn(mp); err != nil {
				_, _ = c.b.zfs(context.WithoutCancel(ctx), []string{"destroy", "-r"}, target)
				return err
			}
			return nil
		})
	}
}

// pickSnapshot: satu salinan dengan awalan ID prefix (harus unik).
func pickSnapshot(snaps []proto.CloudSnapshot, prefix string) (proto.CloudSnapshot, error) {
	var match []proto.CloudSnapshot
	for _, s := range snaps {
		if strings.HasPrefix(s.ID, prefix) {
			match = append(match, s)
		}
	}
	if len(match) != 1 {
		return proto.CloudSnapshot{}, &OpError{Code: proto.ErrNotFound, Message: "salinan tidak ditemukan di repo (atau ID tidak unik)"}
	}
	return match[0], nil
}

// cloudRelPath memeriksa path file/folder di dalam salinan (dari panel):
// relatif, tanpa "."/".." atau komponen kosong. Hasil tanpa "/" di ujung.
func cloudRelPath(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	bad := &OpError{Code: proto.ErrInvalidParams, Message: "path di dalam salinan tidak valid"}
	if len(p) > 4096 || !utf8.ValidString(p) || strings.ContainsAny(p, "\x00\n\r") {
		return "", bad
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", bad
		}
	}
	return p, nil
}

// resticPattern: path sebagai pola --include restic yang hanya cocok dengan
// path itu sendiri (karakter pola di-escape).
func resticPattern(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`\*?[]`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// cachedSnapshots: daftar salinan repo, dari cache bila belum lama.
func (c *Cloud) cachedSnapshots(ctx context.Context, r proto.CloudRepo) ([]proto.CloudSnapshot, error) {
	c.cacheMu.Lock()
	e, ok := c.snapCache[r.ID]
	c.cacheMu.Unlock()
	if ok && time.Since(e.at) < snapCacheTTL {
		return e.list, nil
	}
	list, err := c.listSnapshots(ctx, r)
	if err != nil {
		return nil, err
	}
	c.cacheMu.Lock()
	c.snapCache[r.ID] = cachedSnaps{at: time.Now(), list: list}
	c.cacheMu.Unlock()
	return list, nil
}

// LsOp: cloud.ls. Isi satu folder di dalam salinan (hanya nama, jenis,
// ukuran, waktu; isi file tidak dibaca). Akar salinan diambil dari daftar
// salinan repo, bukan dari panel.
func (c *Cloud) LsOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.CloudLsParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !resticIDPattern.MatchString(p.Snapshot) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "salinan tidak valid"}
		}
		rel, err := cloudRelPath(p.Path)
		if err != nil {
			return nil, err
		}
		st, err := c.get()
		if err != nil {
			return nil, err
		}
		r, ok := st.Repos[p.Repo]
		if !ok {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "repo tidak ditemukan"}
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		snaps, err := c.cachedSnapshots(ctx, *r)
		if err != nil {
			return nil, err
		}
		snap, err := pickSnapshot(snaps, p.Snapshot)
		if err != nil {
			return nil, err
		}
		dir := path.Clean(snap.Path)
		if rel != "" {
			dir = path.Join(dir, rel)
		}
		out, err := c.restic(ctx, *r, modeRead, "ls", "--json", snap.ID, dir)
		if err != nil {
			return nil, resticError(err)
		}
		return parseResticLs(out, dir, c.t.ListMax), nil
	}
}

// parseResticLs: anak langsung dir dari keluaran restic ls --json (baris
// pertama data salinan, lalu satu node per baris; dir sendiri ikut).
func parseResticLs(out []byte, dir string, limit int) proto.CloudLsResult {
	res := proto.CloudLsResult{Entries: []proto.CloudEntry{}}
	for _, line := range bytes.Split(out, []byte("\n")) {
		var n struct {
			Struct string    `json:"struct_type"`
			Name   string    `json:"name"`
			Type   string    `json:"type"`
			Path   string    `json:"path"`
			Size   uint64    `json:"size"`
			MTime  time.Time `json:"mtime"`
		}
		if json.Unmarshal(line, &n) != nil || n.Struct != "node" || path.Dir(n.Path) != dir || path.Base(n.Path) != n.Name || n.Name == "" {
			continue
		}
		if len(res.Entries) >= limit {
			res.Truncated = true
			break
		}
		switch n.Type {
		case "dir", "file", "symlink":
		default:
			n.Type = "other"
		}
		if n.Type == "dir" {
			n.Size = 0
		}
		res.Entries = append(res.Entries, proto.CloudEntry{Name: n.Name, Type: n.Type, Size: n.Size, MTime: n.MTime.UTC()})
	}
	sort.Slice(res.Entries, func(i, j int) bool {
		a, b := res.Entries[i], res.Entries[j]
		if (a.Type == "dir") != (b.Type == "dir") {
			return a.Type == "dir"
		}
		return a.Name < b.Name
	})
	return res
}

// ntpOK: jam tersinkron NTP, atau NTP sengaja dimatikan admin (jam diatur
// manual).
func (c *Cloud) ntpOK(ctx context.Context) bool {
	if c.t.Timedatectl == "" {
		return false
	}
	res, err := runTool(ctx, 20*time.Second, c.t.Timedatectl, []string{"show"})
	if err != nil || res.ExitCode != 0 {
		return false
	}
	props := map[string]string{}
	for _, l := range strings.Split(string(res.Stdout), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			props[k] = strings.TrimSpace(v)
		}
	}
	return props["NTPSynchronized"] == "yes" || props["NTP"] == "no"
}
