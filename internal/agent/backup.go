package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Backup shared folder ke pool lain (ADR 0017). Setiap tugas menyalin satu
// shared folder ke <pool tujuan>/sapo-backup/<id> lewat zfs send/receive:
// salinan pertama penuh, berikutnya hanya perubahannya (-i). Di tujuan,
// data tidak dipasang (canmount=noauto, readonly) dan tanpa penanda shared
// folder, jadi tidak pernah dibagikan atau diubah. Backup tidak pernah
// menghapus data di sumber; yang dibuang hanya snapshot backup lama (sumber:
// kecuali yang terbaru, dasar salinan berikutnya; tujuan: melebihi Keep).

const backupPropName = "sapo:backup"

var backupIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,40}$`)

// BackupTools mengatur backup (agent.backup di sapo.yaml).
type BackupTools struct {
	StateFile string
	// MaxRun: batas waktu satu backup atau pemulihan.
	MaxRun time.Duration
}

// Backups melayani backups.* dan menjalankan jadwalnya.
type Backups struct {
	t      BackupTools
	shares *Shares
	mu     sync.Mutex // state file
	// runMu: satu backup atau pemulihan pada satu waktu.
	runMu   sync.Mutex
	running map[string]bool
	now     func() time.Time
	loc     func() *time.Location
	// pipe menjalankan "zfs send … | zfs receive …"; zfsFn, propsFn, dan
	// poolsFn menjalankan zfs/zpool (semuanya diganti di test).
	pipe    func(ctx context.Context, send, recv []string) (uint64, error)
	zfsFn   func(ctx context.Context, flags []string, operands ...string) (string, error)
	propsFn func(ctx context.Context, ds string, names ...string) (map[string]string, error)
	poolsFn func(ctx context.Context) (map[string]bool, error)
}

// NewBackups menyiapkan backup.
func NewBackups(t BackupTools, shares *Shares) *Backups {
	if t.MaxRun == 0 {
		t.MaxRun = 24 * time.Hour
	}
	b := &Backups{t: t, shares: shares, running: map[string]bool{}, now: time.Now, loc: localZone}
	b.pipe, b.zfsFn, b.propsFn, b.poolsFn = b.zfsPipe, b.runZfs, shares.zfsProps, shares.p.poolNames
	// Pool yang dipakai tugas backup (sumber atau tujuan) tidak dihapus
	// diam-diam.
	shares.p.beforeDestroy = append(shares.p.beforeDestroy, b.refusePool)
	shares.backups = b
	return b
}

type backupJobState struct {
	proto.BackupJob
	LastRun     time.Time `json:"last_run,omitempty"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastOK      bool      `json:"last_ok"`
	LastError   string    `json:"last_error,omitempty"`
	LastBytes   uint64    `json:"last_bytes,omitempty"`
}

type backupState struct {
	Jobs     map[string]*backupJobState `json:"jobs"`
	Restores []proto.BackupRestore      `json:"restores"`
}

func (b *Backups) load() (backupState, error) {
	st := backupState{Jobs: map[string]*backupJobState{}}
	data, err := os.ReadFile(b.t.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("catatan backup rusak (%s): %w", b.t.StateFile, err)
	}
	if st.Jobs == nil {
		st.Jobs = map[string]*backupJobState{}
	}
	return st, nil
}

func (b *Backups) save(st backupState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(b.t.StateFile, data)
}

// update mengubah catatan di bawah kunci.
func (b *Backups) update(fn func(*backupState) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, err := b.load()
	if err != nil {
		return err
	}
	if err := fn(&st); err != nil {
		return err
	}
	return b.save(st)
}

func destDataset(j proto.BackupJob) string { return j.DestPool + "/sapo-backup/" + j.ID }
func srcDataset(j proto.BackupJob) string  { return j.SrcPool + "/" + j.Share }
func backupSnapPrefix(id string) string    { return "sapo-backup-" + id + "_GMT-" }

func validJob(j proto.BackupJob) error {
	switch {
	case !backupIDPattern.MatchString(j.ID):
		return errors.New("ID tugas tidak valid")
	case !poolNamePattern.MatchString(j.SrcPool) || !poolNamePattern.MatchString(j.DestPool):
		return errors.New("pool tidak valid")
	case j.SrcPool == j.DestPool:
		return errors.New("pool tujuan harus berbeda dari pool sumber")
	case !shareNamePattern.MatchString(j.Share):
		return errors.New("shared folder tidak valid")
	case j.Hour < 0 || j.Hour > 23 || j.Keep < 1 || j.Keep > 366:
		return errors.New("jam 0–23 dan jumlah simpan 1–366")
	}
	return nil
}

func (b *Backups) refusePool(_ context.Context, pool string) error {
	st, err := b.loadLocked()
	if err != nil {
		return err
	}
	for _, j := range st.Jobs {
		if j.SrcPool == pool || j.DestPool == pool {
			return &OpError{Code: proto.ErrConflict, Message: "pool " + pool + " masih dipakai tugas backup " + j.ID + "; hapus tugas itu dulu"}
		}
	}
	return nil
}

// usingShare: tugas backup yang memakai shared folder pool/share.
func (b *Backups) usingShare(pool, share string) string {
	st, err := b.loadLocked()
	if err != nil {
		return "?"
	}
	for _, j := range st.Jobs {
		if j.SrcPool == pool && j.Share == share {
			return j.ID
		}
	}
	return ""
}

func (b *Backups) loadLocked() (backupState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.load()
}

func (b *Backups) zfs(ctx context.Context, flags []string, operands ...string) (string, error) {
	return b.zfsFn(ctx, flags, operands...)
}

func (b *Backups) runZfs(ctx context.Context, flags []string, operands ...string) (string, error) {
	sh := b.shares
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := sh.p.st.run(ctx, sh.t.Zfs, flags, operands...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", errors.New(clipTo(firstLine(res.Stderr), maxErrorText))
	}
	return string(res.Stdout), nil
}

// zfsPipe menjalankan zfs send dan zfs receive bersambung (tanpa shell),
// menghitung byte yang dikirim.
func (b *Backups) zfsPipe(ctx context.Context, send, recv []string) (uint64, error) {
	zfs := b.shares.t.Zfs
	sc := exec.CommandContext(ctx, zfs, send...)
	rc := exec.CommandContext(ctx, zfs, recv...)
	sendErr, recvErr := &limitedBuffer{max: 4096}, &limitedBuffer{max: 4096}
	sc.Stderr, rc.Stderr = sendErr, recvErr
	out, err := sc.StdoutPipe()
	if err != nil {
		return 0, err
	}
	in, err := rc.StdinPipe()
	if err != nil {
		return 0, err
	}
	if err := rc.Start(); err != nil {
		return 0, err
	}
	if err := sc.Start(); err != nil {
		in.Close()
		_ = rc.Wait()
		return 0, err
	}
	n, copyErr := io.Copy(in, out)
	in.Close()
	sErr, rErr := sc.Wait(), rc.Wait()
	switch {
	case rErr != nil:
		return uint64(n), fmt.Errorf("zfs receive: %s", firstLine(recvErr.Bytes()))
	case sErr != nil:
		return uint64(n), fmt.Errorf("zfs send: %s", firstLine(sendErr.Bytes()))
	case copyErr != nil:
		return uint64(n), copyErr
	}
	return uint64(n), nil
}

// snapshots: snapshot dataset ds yang namanya berawalan prefix, terlama dulu.
func (b *Backups) snapshots(ctx context.Context, ds, prefix string) ([]proto.BackupCopy, error) {
	// Urutan menurut createtxg (urutan transaksi di pool ini): waktu
	// creation snapshot hasil receive berasal dari stream dan bisa dipalsukan.
	out, err := b.zfs(ctx, []string{"list", "-H", "-p", "-t", "snapshot", "-d", "1", "-o", "name,creation,createtxg"}, ds)
	if err != nil {
		return nil, err
	}
	type entry struct {
		c   proto.BackupCopy
		txg uint64
	}
	var list []entry
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 3 {
			continue
		}
		_, name, ok := strings.Cut(f[0], "@")
		if !ok || !strings.HasPrefix(name, prefix) {
			continue
		}
		sec, _ := strconv.ParseInt(f[1], 10, 64)
		txg, _ := strconv.ParseUint(f[2], 10, 64)
		list = append(list, entry{proto.BackupCopy{Name: name, Created: time.Unix(sec, 0).UTC()}, txg})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].txg < list[j].txg })
	res := make([]proto.BackupCopy, 0, len(list))
	for _, e := range list {
		res = append(res, e.c)
	}
	return res, nil
}

// checkSource: dataset sumber adalah shared folder Sapo.
func (b *Backups) checkSource(ctx context.Context, j proto.BackupJob) error {
	props, err := b.propsFn(ctx, srcDataset(j), "type", sharePropName)
	if errors.Is(err, errNoDataset) || (err == nil && (props["type"] != "filesystem" || props[sharePropName] != j.Share)) {
		return &OpError{Code: proto.ErrNotFound, Message: "shared folder " + srcDataset(j) + " tidak ditemukan"}
	}
	return err
}

// destState: "" (belum ada), "ok" (salinan tugas ini), atau galat (dataset
// bernama sama yang bukan milik tugas ini).
func (b *Backups) destState(ctx context.Context, j proto.BackupJob) (bool, error) {
	props, err := b.propsFn(ctx, destDataset(j), "type", backupPropName)
	if errors.Is(err, errNoDataset) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if props["type"] != "filesystem" || props[backupPropName] != j.ID {
		return false, &OpError{Code: proto.ErrConflict, Message: "dataset " + destDataset(j) + " sudah ada dan bukan salinan tugas ini"}
	}
	return true, nil
}

// ensureParent: <pool tujuan>/sapo-backup (penanda @induk, tidak dipasang).
func (b *Backups) ensureParent(ctx context.Context, pool string) error {
	ds := pool + "/sapo-backup"
	props, err := b.propsFn(ctx, ds, backupPropName, sharePropName)
	if errors.Is(err, errNoDataset) {
		_, err = b.zfs(ctx, []string{"create", "-o", backupPropName + "=" + parentMarker, "-o", "canmount=off", "-o", "devices=off", "-o", "setuid=off"}, ds)
		return err
	}
	if err != nil {
		return err
	}
	if props[backupPropName] != parentMarker || (props[sharePropName] != "-" && props[sharePropName] != "") {
		return &OpError{Code: proto.ErrConflict, Message: "dataset " + ds + " sudah ada dan bukan milik backup Sapo"}
	}
	return nil
}

// runJob menjalankan satu backup. Kunci pool hanya dipegang sebentar untuk
// membuat/membuang snapshot; pengiriman data berjalan tanpa kunci pool.
func (b *Backups) runJob(ctx context.Context, j proto.BackupJob) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, b.t.MaxRun)
	defer cancel()
	if err := b.checkSource(ctx, j); err != nil {
		return 0, err
	}
	pools, err := b.poolsFn(ctx)
	if err != nil {
		return 0, err
	}
	if !pools[j.DestPool] {
		return 0, &OpError{Code: proto.ErrNotFound, Message: "pool tujuan " + j.DestPool + " tidak ditemukan (drive dicabut?)"}
	}
	if err := b.ensureParent(ctx, j.DestPool); err != nil {
		return 0, err
	}
	exists, err := b.destState(ctx, j)
	if err != nil {
		return 0, err
	}
	src, dst := srcDataset(j), destDataset(j)
	prefix := backupSnapPrefix(j.ID)
	var base string
	if exists {
		have, err := b.snapshots(ctx, dst, prefix)
		if err != nil {
			return 0, err
		}
		srcSnaps, err := b.snapshots(ctx, src, prefix)
		if err != nil {
			return 0, err
		}
		// Dasar harus snapshot TERBARU di tujuan (receive -i tanpa -F menolak
		// dasar lain) dan masih ada di sumber.
		if n := len(have); n > 0 && slices.ContainsFunc(srcSnaps, func(c proto.BackupCopy) bool { return c.Name == have[n-1].Name }) {
			base = have[n-1].Name
		}
		if base == "" {
			// Tidak pernah ditimpa otomatis (data dulu).
			return 0, &OpError{Code: "backup_diverged", Message: "salinan di tujuan tidak punya dasar yang sama dengan sumber; hapus salinan lama lalu backup ulang"}
		}
	}
	snap := prefix + b.now().UTC().Format(snapTimeFormat)
	if err := b.locked(ctx, func() error {
		_, err := b.zfs(ctx, []string{"snapshot", "-o", snapAutoProp + "=backup"}, src+"@"+snap)
		return err
	}); err != nil {
		return 0, err
	}
	var n uint64
	if base == "" {
		n, err = b.pipe(ctx, []string{"send", "-L", "-c", "--", src + "@" + snap},
			[]string{"receive", "-u", "-o", "canmount=noauto", "-o", "readonly=on", "-o", backupPropName + "=" + j.ID, "--", dst})
	} else {
		n, err = b.pipe(ctx, []string{"send", "-L", "-c", "-i", "@" + base, "--", src + "@" + snap},
			[]string{"receive", "-u", "--", dst})
	}
	if err != nil {
		// Stream bisa saja sudah diterima utuh walau salah satu proses
		// melapor gagal di akhir: bila snapshot itu ada di tujuan, ia dasar
		// berikutnya dan tidak boleh dibuang dari sumber.
		if got, lerr := b.snapshots(context.WithoutCancel(ctx), dst, prefix); lerr == nil && slices.ContainsFunc(got, func(c proto.BackupCopy) bool { return c.Name == snap }) {
			b.shares.p.logWarn("backup melapor gagal tetapi salinan diterima", "tugas", j.ID, "err", err)
		} else {
			b.discardSnap(ctx, src+"@"+snap)
		}
		return n, err
	}
	_ = b.locked(ctx, func() error { b.prune(ctx, j, src, dst, snap); return nil })
	b.shares.p.logInfo("backup selesai", "tugas", j.ID, "byte", n, "salinan", snap)
	return n, nil
}

// discardSnap membuang snapshot backup yang gagal dikirim (dasar lama tetap).
func (b *Backups) discardSnap(ctx context.Context, name string) {
	ctx = context.WithoutCancel(ctx)
	_ = b.locked(ctx, func() error {
		_, err := b.zfs(ctx, []string{"destroy"}, name)
		return err
	})
}

// prune membuang snapshot backup lama: di sumber semua kecuali snap (dasar
// salinan berikutnya), di tujuan yang melebihi Keep.
func (b *Backups) prune(ctx context.Context, j proto.BackupJob, src, dst, snap string) {
	prefix := backupSnapPrefix(j.ID)
	// Sumber: hanya snapshot backup terbaru yang disimpan (dasar berikutnya).
	if old, err := b.snapshots(ctx, src, prefix); err == nil {
		var names []string
		for _, c := range old {
			if c.Name != snap {
				names = append(names, c.Name)
			}
		}
		if len(names) > 0 {
			_, _ = b.zfs(ctx, []string{"destroy"}, src+"@"+strings.Join(names, ","))
		}
	}
	// Tujuan: Keep salinan terbaru.
	if have, err := b.snapshots(ctx, dst, prefix); err == nil && len(have) > j.Keep {
		var names []string
		for _, c := range have[:len(have)-j.Keep] {
			names = append(names, c.Name)
		}
		_, _ = b.zfs(ctx, []string{"destroy"}, dst+"@"+strings.Join(names, ","))
	}
}

// locked menjalankan fn sambil memegang kunci pool; menunggu (paling lama
// sampai ctx selesai) bila operasi pool lain sedang berjalan.
func (b *Backups) locked(ctx context.Context, fn func() error) error {
	for !b.shares.p.write.TryLock() {
		select {
		case <-ctx.Done():
			return &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		case <-time.After(time.Second):
		}
	}
	defer b.shares.p.write.Unlock()
	return fn()
}

// execute menjalankan tugas id dan mencatat hasilnya.
func (b *Backups) execute(ctx context.Context, id string) error {
	if !b.runMu.TryLock() {
		return &OpError{Code: proto.ErrBusy, Message: "backup atau pemulihan lain sedang berjalan"}
	}
	defer b.runMu.Unlock()
	st, err := b.loadLocked()
	if err != nil {
		return err
	}
	js, ok := st.Jobs[id]
	if !ok {
		return &OpError{Code: proto.ErrNotFound, Message: "tugas backup tidak ditemukan"}
	}
	b.setRunning(id, true)
	defer b.setRunning(id, false)
	start := b.now().UTC()
	n, runErr := b.runJob(ctx, js.BackupJob)
	err = b.update(func(st *backupState) error {
		j, ok := st.Jobs[id]
		if !ok {
			return nil
		}
		j.LastAttempt, j.LastOK, j.LastBytes = start, runErr == nil, n
		j.LastError = ""
		if runErr != nil {
			j.LastError = clipTo(runErr.Error(), maxErrorText)
		} else {
			j.LastRun = start
		}
		return nil
	})
	if runErr != nil {
		b.shares.p.logWarn("backup gagal", "tugas", id, "err", runErr)
		return runErr
	}
	return err
}

func (b *Backups) setRunning(id string, on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if on {
		b.running[id] = true
	} else {
		delete(b.running, id)
	}
}

// due: jadwal harian sudah lewat hari ini dan belum ada backup berhasil
// sejak itu; percobaan yang gagal diulang paling cepat sejam kemudian.
func (b *Backups) due(j *backupJobState, now time.Time) bool {
	if !j.Enabled {
		return false
	}
	start := periodStart("daily", proto.SnapshotPolicy{Hour: j.Hour}, now, b.loc())
	if !j.LastRun.Before(start) {
		return false
	}
	return j.LastAttempt.IsZero() || now.Sub(j.LastAttempt) >= time.Hour || j.LastAttempt.Before(start)
}

// Run memeriksa jadwal setiap menit sampai ctx selesai.
func (b *Backups) Run(ctx context.Context) {
	// Pemulihan yang terputus karena sapod berhenti tidak akan selesai.
	_ = b.update(func(st *backupState) error {
		for i := range st.Restores {
			if r := &st.Restores[i]; r.Running {
				r.Running, r.OK, r.Finished, r.Error = false, false, b.now().UTC(), "terputus sebelum selesai (sapod berhenti)"
			}
		}
		return nil
	})
	for {
		if st, err := b.loadLocked(); err == nil {
			ids := make([]string, 0, len(st.Jobs))
			for id := range st.Jobs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				if b.due(st.Jobs[id], b.now()) {
					_ = b.execute(ctx, id)
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

// --- Handler -----------------------------------------------------------------

// ListOp mengembalikan handler backups.list.
func (b *Backups) ListOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		st, err := b.loadLocked()
		if err != nil {
			return nil, err
		}
		out := proto.BackupsListResult{Jobs: []proto.BackupStatus{}, Restores: []proto.BackupRestore{}}
		for _, r := range st.Restores {
			if r.OK {
				props, err := b.propsFn(ctx, r.Pool+"/"+r.Name, sharePropName)
				r.Present = err == nil && props[sharePropName] == r.Name
			}
			out.Restores = append(out.Restores, r)
		}
		ids := make([]string, 0, len(st.Jobs))
		for id := range st.Jobs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		b.mu.Lock()
		running := maps(b.running)
		b.mu.Unlock()
		for _, id := range ids {
			j := st.Jobs[id]
			s := proto.BackupStatus{BackupJob: j.BackupJob, Running: running[id], LastRun: j.LastRun, LastOK: j.LastOK,
				LastError: j.LastError, LastBytes: j.LastBytes, Copies: []proto.BackupCopy{}}
			if exists, err := b.destState(ctx, j.BackupJob); err == nil && exists {
				s.DestExists = true
				if c, err := b.snapshots(ctx, destDataset(j.BackupJob), backupSnapPrefix(id)); err == nil {
					s.Copies = c
				}
				if props, err := b.propsFn(ctx, destDataset(j.BackupJob), "used"); err == nil {
					s.UsedBytes, _ = strconv.ParseUint(props["used"], 10, 64)
				}
			}
			out.Jobs = append(out.Jobs, s)
		}
		return out, nil
	}
}

func maps(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SetOp mengembalikan handler backups.set: membuat atau mengubah tugas.
// Sumber dan pool tujuan tugas yang sudah ada tidak bisa diubah (salinannya
// terikat ke keduanya); hanya jadwal, jumlah simpan, dan nyala/mati.
func (b *Backups) SetOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var j proto.BackupJob
		if err := decodeParams(raw, &j); err != nil {
			return nil, err
		}
		if err := validJob(j); err != nil {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		if err := b.checkSource(ctx, j); err != nil {
			return nil, err
		}
		pools, err := b.poolsFn(ctx)
		if err != nil {
			return nil, err
		}
		if !pools[j.DestPool] {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "pool tujuan tidak ditemukan"}
		}
		err = b.update(func(st *backupState) error {
			if cur, ok := st.Jobs[j.ID]; ok {
				if cur.SrcPool != j.SrcPool || cur.Share != j.Share || cur.DestPool != j.DestPool {
					return &OpError{Code: proto.ErrConflict, Message: "sumber dan tujuan tugas yang sudah ada tidak bisa diubah"}
				}
				cur.Hour, cur.Keep, cur.Enabled = j.Hour, j.Keep, j.Enabled
				return nil
			}
			for _, o := range st.Jobs {
				if o.SrcPool == j.SrcPool && o.Share == j.Share && o.DestPool == j.DestPool {
					return &OpError{Code: proto.ErrConflict, Message: "folder ini sudah di-backup ke pool itu (" + o.ID + ")"}
				}
			}
			st.Jobs[j.ID] = &backupJobState{BackupJob: j}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	}
}

// RunOp mengembalikan handler backups.run: backup sekarang (di latar
// belakang).
func (b *Backups) RunOp() Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var p proto.BackupDeleteParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		st, err := b.loadLocked()
		if err != nil {
			return nil, err
		}
		if _, ok := st.Jobs[p.ID]; !ok {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "tugas backup tidak ditemukan"}
		}
		if !b.runMu.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "backup atau pemulihan lain sedang berjalan"}
		}
		b.runMu.Unlock()
		go func() { _ = b.execute(context.Background(), p.ID) }()
		return map[string]bool{"ok": true}, nil
	}
}

// DeleteOp mengembalikan handler backups.delete. DeleteData juga menghapus
// semua salinan di tujuan (destruktif; sapo-api meminta password).
func (b *Backups) DeleteOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.BackupDeleteParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !b.runMu.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "backup atau pemulihan sedang berjalan"}
		}
		defer b.runMu.Unlock()
		st, err := b.loadLocked()
		if err != nil {
			return nil, err
		}
		js, ok := st.Jobs[p.ID]
		if !ok {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "tugas backup tidak ditemukan"}
		}
		j := js.BackupJob
		if p.DeleteData {
			exists, err := b.destState(ctx, j)
			if err != nil {
				return nil, err
			}
			if exists {
				if _, err := b.zfs(ctx, []string{"destroy", "-r"}, destDataset(j)); err != nil {
					return nil, &OpError{Code: proto.ErrPoolBusy, Message: err.Error()}
				}
			}
		}
		// Snapshot backup di sumber (dasar salinan) dibuang.
		if snaps, err := b.snapshots(ctx, srcDataset(j), backupSnapPrefix(j.ID)); err == nil && len(snaps) > 0 {
			var names []string
			for _, c := range snaps {
				names = append(names, c.Name)
			}
			_, _ = b.zfs(ctx, []string{"destroy"}, srcDataset(j)+"@"+strings.Join(names, ","))
		}
		if err := b.update(func(st *backupState) error { delete(st.Jobs, p.ID); return nil }); err != nil {
			return nil, err
		}
		b.shares.p.logInfo("tugas backup dihapus", "tugas", p.ID, "salinan_dihapus", p.DeleteData)
		return map[string]bool{"ok": true}, nil
	}
}

// RestoreOp mengembalikan handler backups.restore: salinan dipulihkan
// menjadi shared folder BARU di pool sumber (folder yang ada tidak pernah
// ditimpa), di latar belakang. sapo-api menambahkan folder itu ke daftar
// setelah selesai.
func (b *Backups) RestoreOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.BackupRestoreParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !shareNamePattern.MatchString(p.Name) || slices.Contains(reservedShares, p.Name) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama folder tidak valid"}
		}
		st, err := b.loadLocked()
		if err != nil {
			return nil, err
		}
		js, ok := st.Jobs[p.ID]
		if !ok {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "tugas backup tidak ditemukan"}
		}
		j := js.BackupJob
		if !strings.HasPrefix(p.Copy, backupSnapPrefix(j.ID)) || !snapNameTail.MatchString(strings.TrimPrefix(p.Copy, backupSnapPrefix(j.ID))) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "salinan tidak valid"}
		}
		// Salinan harus milik tugas ini (pool tujuan bisa drive lain bernama
		// sama).
		if exists, err := b.destState(ctx, j); err != nil {
			return nil, err
		} else if !exists {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "salinan tugas ini tidak ada di pool " + j.DestPool}
		}
		if err := b.startRestore(ctx, j.ID, destDataset(j), p.Copy, j.SrcPool, p.Name); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	}
}

// startRestore memulihkan snapshot srcDS@copy menjadi shared folder BARU
// pool/name di latar belakang (folder yang ada tidak pernah ditimpa) dan
// mencatatnya untuk sapo-api (backups.list/claim). jobID: tugas asal (untuk
// izin folder asal di sapo-api), "remote:…" untuk salinan dari NAS lain.
func (b *Backups) startRestore(ctx context.Context, jobID, srcDS, copyName, pool, name string) error {
	target := pool + "/" + name
	if _, err := b.propsFn(ctx, target, "type"); !errors.Is(err, errNoDataset) {
		return &OpError{Code: proto.ErrConflict, Message: "folder " + name + " sudah ada; pilih nama lain"}
	}
	if !b.runMu.TryLock() {
		return &OpError{Code: proto.ErrBusy, Message: "backup atau pemulihan lain sedang berjalan"}
	}
	rec := proto.BackupRestore{JobID: jobID, Copy: copyName, Pool: pool, Name: name, Started: b.now().UTC(), Running: true}
	if err := b.update(func(st *backupState) error {
		st.Restores = append(st.Restores, rec)
		// Catatan lama dibuang, kecuali pemulihan berhasil yang belum
		// didaftarkan sapo-api.
		for len(st.Restores) > 20 {
			i := slices.IndexFunc(st.Restores, func(r proto.BackupRestore) bool { return !r.Running && (r.Claimed || !r.OK) })
			if i < 0 {
				break
			}
			st.Restores = slices.Delete(st.Restores, i, i+1)
		}
		return nil
	}); err != nil {
		b.runMu.Unlock()
		return err
	}
	go func() {
		defer b.runMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), b.t.MaxRun)
		defer cancel()
		_, err := b.pipe(ctx, []string{"send", "-L", "-c", "--", srcDS + "@" + copyName},
			[]string{"receive", "-u", "-o", sharePropName + "=" + name, "-o", "devices=off", "-o", "setuid=off", "--", target})
		if err == nil {
			// Snapshot salinan yang ikut terbawa tidak diperlukan folder baru.
			_, _ = b.zfs(ctx, []string{"destroy"}, target+"@"+copyName)
			_, err = b.zfs(ctx, []string{"mount"}, target)
		}
		_ = b.update(func(st *backupState) error {
			for i := range st.Restores {
				r := &st.Restores[i]
				if r.Running && r.Pool == rec.Pool && r.Name == rec.Name {
					r.Running, r.OK, r.Finished = false, err == nil, b.now().UTC()
					if err != nil {
						r.Error = clipTo(err.Error(), maxErrorText)
					}
				}
			}
			return nil
		})
		if err != nil {
			b.shares.p.logWarn("pemulihan backup gagal", "tugas", jobID, "err", err)
		} else {
			b.shares.p.logInfo("backup dipulihkan sebagai folder baru", "tugas", jobID, "folder", target)
		}
	}()
	return nil
}

// ClaimOp mengembalikan handler backups.claim: sapo-api mengklaim folder
// pulihan Pool/Name untuk didaftarkan. Berhasil sekali saja, dan hanya bila
// pemulihan selesai dan datasetnya masih shared folder itu; jadi folder yang
// sudah didaftarkan lalu dihapus admin tidak pernah didaftarkan ulang.
func (b *Backups) ClaimOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.BackupClaimParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		claimed := false
		err := b.update(func(st *backupState) error {
			for i := range st.Restores {
				r := &st.Restores[i]
				if r.Pool != p.Pool || r.Name != p.Name || !r.OK || r.Running || r.Claimed {
					continue
				}
				props, err := b.propsFn(ctx, r.Pool+"/"+r.Name, sharePropName)
				if err == nil && props[sharePropName] == r.Name {
					r.Claimed, claimed = true, true
				}
				return nil
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]bool{"claimed": claimed}, nil
	}
}

var snapNameTail = regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2}$`)
