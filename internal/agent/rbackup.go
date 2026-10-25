package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Backup ke NAS lain (ADR 0019). sapod tidak pernah ke jaringan: sapo-api
// membawa data lewat HTTPS, sapod hanya menjalankan zfs send / zfs receive
// dan menukar ujung pipa dengan sapo-api lewat file descriptor.
//
// Penerima menyimpan salinan di <pool>/sapo-backup-remote/<partner>/<job>
// (tidak dipasang, readonly, penanda sapo:backup=remote:<partner>/<job>);
// partner hanya bisa menulis ke dataset miliknya, dan dasar inkremental
// harus snapshot terbaru di penerima (tanpa receive -F).

var partnerIDPattern = regexp.MustCompile(`^[a-z0-9]{8,32}$`)

const (
	// Sesi kirim dan terima dihitung terpisah: partner yang lambat tidak
	// menahan backup keluar NAS ini.
	maxRecvSessions = 4
	maxSendSessions = 2
	// maxReceivesPerDay: salinan per tugas per 24 jam (jam penerima).
	maxReceivesPerDay = 4
	// maxPartnerJobs: tugas (dataset) per partner.
	maxPartnerJobs = 50
	receivedProp   = "sapo:received"
)

// RemoteBackups melayani rbackup.*.
type RemoteBackups struct {
	b        *Backups
	mu       sync.Mutex
	sessions map[string]*pipeSession
	// spawn menjalankan zfs dengan satu ujung pipa (in: zfs membaca dari
	// pipa, file yang dikembalikan untuk ditulis; sebaliknya untuk out).
	// Diganti di test.
	spawn func(ctx context.Context, args []string, in bool) (*os.File, func() error, error)
}

type pipeSession struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	// Untuk penyelesaian: dataset dan snapshot aliran ini.
	recv     bool
	ds, snap string
	prefix   string
	expires  time.Time
}

// NewRemoteBackups menyiapkan backup jarak jauh di atas b.
func NewRemoteBackups(b *Backups) *RemoteBackups {
	r := &RemoteBackups{b: b, sessions: map[string]*pipeSession{}}
	r.spawn = r.zfsSpawn
	// Pool yang menyimpan salinan dari NAS lain tidak dihapus diam-diam.
	b.shares.p.beforeDestroy = append(b.shares.p.beforeDestroy, r.refusePool)
	return r
}

func rbackupPrefix(job string) string { return "sapo-rbackup-" + job + "_GMT-" }

func remoteDS(t proto.RBackupTarget) string {
	return t.Pool + "/sapo-backup-remote/" + t.Partner + "/" + t.Job
}

func remoteMarker(t proto.RBackupTarget) string { return "remote:" + t.Partner + "/" + t.Job }

func validTarget(t proto.RBackupTarget, needJob bool) error {
	switch {
	case !poolNamePattern.MatchString(t.Pool):
		return errors.New("pool tidak valid")
	case !partnerIDPattern.MatchString(t.Partner):
		return errors.New("partner tidak valid")
	case (needJob || t.Job != "") && !backupIDPattern.MatchString(t.Job):
		return errors.New("tugas tidak valid")
	}
	return nil
}

func validCopyName(job, name string) bool {
	tail, ok := strings.CutPrefix(name, rbackupPrefix(job))
	return ok && snapNameTail.MatchString(tail)
}

func (r *RemoteBackups) zfsSpawn(ctx context.Context, args []string, in bool) (*os.File, func() error, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, r.b.shares.t.Zfs, args...)
	stderr := &limitedBuffer{max: 4096}
	cmd.Stderr = stderr
	mine, theirs := pw, pr
	if in {
		cmd.Stdin = pr
	} else {
		cmd.Stdout = pw
		mine, theirs = pr, pw
	}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, nil, err
	}
	theirs.Close()
	wait := func() error {
		if err := cmd.Wait(); err != nil {
			if msg := firstLine(stderr.Bytes()); msg != "" {
				return fmt.Errorf("zfs %s: %s", args[0], msg)
			}
			return fmt.Errorf("zfs %s: %w", args[0], err)
		}
		return nil
	}
	return mine, wait, nil
}

// open memulai aliran dan mencatat sesinya. Sesi yang tidak pernah
// diselesaikan (sapo-api berhenti) dihentikan setelah MaxRun.
func (r *RemoteBackups) open(args []string, in bool, sess *pipeSession) (string, *os.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.b.now()
	if err := r.reapLocked(now, sess.recv); err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.b.t.MaxRun)
	f, wait, err := r.spawn(ctx, args, in)
	if err != nil {
		cancel()
		return "", nil, err
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	id := hex.EncodeToString(raw)
	sess.cancel, sess.done, sess.expires = cancel, make(chan struct{}), now.Add(r.b.t.MaxRun)
	go func() {
		sess.err = wait()
		close(sess.done)
	}()
	r.sessions[id] = sess
	return id, f, nil
}

// reapLocked menghentikan sesi kedaluwarsa (snapshot kirimnya dibuang) dan
// memeriksa batas sesi jenis itu.
func (r *RemoteBackups) reapLocked(now time.Time, recv bool) error {
	n := 0
	for id, s := range r.sessions {
		if now.After(s.expires) {
			s.cancel()
			delete(r.sessions, id)
			if !s.recv {
				go r.b.discardSnap(context.Background(), s.ds+"@"+s.snap)
			}
			continue
		}
		if s.recv == recv {
			n++
		}
	}
	limit := maxSendSessions
	if recv {
		limit = maxRecvSessions
	}
	if n >= limit {
		return &OpError{Code: proto.ErrBusy, Message: "terlalu banyak backup jarak jauh yang berjalan"}
	}
	return nil
}

// openRecv membuka sesi penerima: sapo-api menulis ke pipa perantara;
// header stream diperiksa (checkStreamHeader) SEBELUM zfs receive
// dijalankan, lalu sisanya diteruskan.
func (r *RemoteBackups) openRecv(args []string, incremental bool, sess *pipeSession) (string, *os.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.b.now()
	if err := r.reapLocked(now, true); err != nil {
		return "", nil, err
	}
	ar, aw, err := os.Pipe()
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.b.t.MaxRun)
	sess.cancel, sess.done, sess.expires = cancel, make(chan struct{}), now.Add(r.b.t.MaxRun)
	go func() {
		defer close(sess.done)
		defer ar.Close()
		// Ditutup saat dibatalkan agar pembacaan header tidak menggantung.
		stop := context.AfterFunc(ctx, func() { ar.Close() })
		defer stop()
		hdr, err := checkStreamHeader(ar, incremental)
		if err != nil {
			sess.err = err
			return
		}
		zw, wait, err := r.spawn(ctx, args, true)
		if err != nil {
			sess.err = err
			return
		}
		_, werr := zw.Write(hdr)
		if werr == nil {
			_, werr = io.Copy(zw, ar)
		}
		zw.Close()
		if err := wait(); err != nil {
			sess.err = err
		} else if werr != nil {
			sess.err = werr
		}
	}()
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	id := hex.EncodeToString(raw)
	r.sessions[id] = sess
	return id, aw, nil
}

func (r *RemoteBackups) take(id string) (*pipeSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil, &OpError{Code: proto.ErrNotFound, Message: "sesi backup tidak ditemukan"}
	}
	delete(r.sessions, id)
	return s, nil
}

// finish menunggu proses zfs selesai (atau menghentikannya bila !ok).
func (s *pipeSession) finish(ok bool) error {
	if !ok {
		s.cancel()
	}
	<-s.done
	s.cancel()
	if !ok && s.err == nil {
		return errors.New("aliran terputus")
	}
	return s.err
}

func (r *RemoteBackups) refusePool(ctx context.Context, pool string) error {
	if _, err := r.b.propsFn(ctx, pool+"/sapo-backup-remote", "type"); errors.Is(err, errNoDataset) {
		return nil // tidak ada salinan dari NAS lain
	} else if err != nil {
		return err // gagal-tertutup
	}
	out, err := r.b.zfs(ctx, []string{"list", "-H", "-o", "name", "-r", "-d", "2"}, pool+"/sapo-backup-remote")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Count(line, "/") >= 2 {
			return &OpError{Code: proto.ErrConflict, Message: "pool " + pool + " menyimpan backup dari NAS lain; hapus partner itu dulu"}
		}
	}
	return nil
}

// ensureRemoteParents: <pool>/sapo-backup-remote dan .../<partner>
// (penanda @induk, tidak dipasang).
func (r *RemoteBackups) ensureRemoteParents(ctx context.Context, t proto.RBackupTarget, quota uint64) error {
	partnerDS := t.Pool + "/sapo-backup-remote/" + t.Partner
	for _, ds := range []string{t.Pool + "/sapo-backup-remote", partnerDS} {
		props, err := r.b.propsFn(ctx, ds, backupPropName, sharePropName)
		if errors.Is(err, errNoDataset) {
			if _, err := r.b.zfs(ctx, []string{"create", "-o", backupPropName + "=" + parentMarker, "-o", "canmount=off", "-o", "devices=off", "-o", "setuid=off", "-o", "exec=off"}, ds); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if props[backupPropName] != parentMarker || (props[sharePropName] != "-" && props[sharePropName] != "") {
			return &OpError{Code: proto.ErrConflict, Message: "dataset " + ds + " sudah ada dan bukan milik backup Sapo"}
		}
	}
	// Kuota partner (0 = tanpa batas); gagal memasangnya = tidak menerima.
	q := "none"
	if quota > 0 {
		q = strconv.FormatUint(quota, 10)
	}
	if _, err := r.b.zfs(ctx, []string{"set", "quota=" + q}, partnerDS); err != nil {
		return fmt.Errorf("kuota partner tidak bisa dipasang: %w", err)
	}
	return nil
}

// remoteState: salinan tugas itu sudah ada (dan memang miliknya).
func (r *RemoteBackups) remoteState(ctx context.Context, t proto.RBackupTarget) (bool, error) {
	props, err := r.b.propsFn(ctx, remoteDS(t), "type", backupPropName)
	if errors.Is(err, errNoDataset) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if props["type"] != "filesystem" || props[backupPropName] != remoteMarker(t) {
		return false, &OpError{Code: proto.ErrConflict, Message: "dataset " + remoteDS(t) + " bukan salinan tugas ini"}
	}
	return true, nil
}

// receivedTimes: salinan (prefix) di ds → waktu diterima (sapo:received;
// salinan tanpa catatan dianggap lama).
func (r *RemoteBackups) receivedTimes(ctx context.Context, ds, prefix string) (map[string]time.Time, error) {
	out, err := r.b.zfs(ctx, []string{"list", "-H", "-p", "-t", "snapshot", "-d", "1", "-o", "name," + receivedProp}, ds)
	if err != nil {
		return nil, err
	}
	m := map[string]time.Time{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 2 {
			continue
		}
		_, name, ok := strings.Cut(f[0], "@")
		if !ok || !strings.HasPrefix(name, prefix) {
			continue
		}
		// Tanpa catatan (misalnya gagal ditulis) dianggap BARU: disimpan dan
		// dihitung dalam batas harian, bukan dibuang.
		sec, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || sec <= 0 {
			m[name] = r.b.now()
			continue
		}
		m[name] = time.Unix(sec, 0)
	}
	return m, nil
}

// withReceived mengganti waktu salinan dengan waktu terima (jam penerima),
// bukan waktu dari stream.
func (r *RemoteBackups) withReceived(ctx context.Context, ds, prefix string, copies []proto.BackupCopy) {
	recv, err := r.receivedTimes(ctx, ds, prefix)
	if err != nil {
		return
	}
	for i := range copies {
		if at, ok := recv[copies[i].Name]; ok {
			copies[i].Created = at.UTC()
		}
	}
}

// partnerJobs: jumlah tugas (dataset) partner di penerima.
func (r *RemoteBackups) partnerJobs(ctx context.Context, t proto.RBackupTarget) (int, error) {
	out, err := r.b.zfs(ctx, []string{"list", "-H", "-o", "name", "-d", "1", "-t", "filesystem"}, t.Pool+"/sapo-backup-remote/"+t.Partner)
	if err != nil {
		return 0, err
	}
	return max(len(strings.Split(strings.TrimSpace(out), "\n"))-1, 0), nil
}

// --- Penerima ------------------------------------------------------------------

// RecvOpenOp: rbackup.recv_open.
func (r *RemoteBackups) RecvOpenOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.RBackupRecvParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := validTarget(p.RBackupTarget, true); err != nil {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		if !validCopyName(p.Job, p.Snap) || (p.Base != "" && !validCopyName(p.Job, p.Base)) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama salinan tidak valid"}
		}
		pools, err := r.b.poolsFn(ctx)
		if err != nil {
			return nil, err
		}
		if !pools[p.Pool] {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "pool penerima tidak ditemukan"}
		}
		if err := r.ensureRemoteParents(ctx, p.RBackupTarget, p.QuotaBytes); err != nil {
			return nil, err
		}
		exists, err := r.remoteState(ctx, p.RBackupTarget)
		if err != nil {
			return nil, err
		}
		ds := remoteDS(p.RBackupTarget)
		// Properti berbahaya dipaksa di setiap penerimaan (lapis kedua; stream
		// gabungan yang bisa membawa properti sudah ditolak checkStreamHeader).
		guard := []string{"-o", "canmount=noauto", "-o", "readonly=on",
			"-x", "mountpoint", "-x", "sharenfs", "-x", "sharesmb", "-x", "setuid", "-x", "exec", "-x", "devices"}
		args := append([]string{"receive", "-u"}, guard...)
		switch {
		case p.Base == "" && exists:
			return nil, &OpError{Code: "backup_diverged", Message: "salinan tugas ini sudah ada di penerima; kirim inkremental dari salinan terbarunya"}
		case p.Base == "":
			if n, err := r.partnerJobs(ctx, p.RBackupTarget); err != nil {
				return nil, err
			} else if n >= maxPartnerJobs {
				return nil, &OpError{Code: proto.ErrConflict, Message: "batas jumlah folder per NAS pengirim tercapai"}
			}
			args = append(args, "-o", backupPropName+"="+remoteMarker(p.RBackupTarget))
		case !exists:
			return nil, &OpError{Code: "backup_diverged", Message: "salinan dasar tidak ada di penerima"}
		default:
			have, err := r.b.snapshots(ctx, ds, rbackupPrefix(p.Job))
			if err != nil {
				return nil, err
			}
			if len(have) == 0 || have[len(have)-1].Name != p.Base {
				return nil, &OpError{Code: "backup_diverged", Message: "dasar inkremental bukan salinan terbaru di penerima"}
			}
			// Batas penerimaan harian (jam penerima).
			recv, err := r.receivedTimes(ctx, ds, rbackupPrefix(p.Job))
			if err != nil {
				return nil, err
			}
			today := 0
			for _, at := range recv {
				if r.b.now().Sub(at) < 24*time.Hour {
					today++
				}
			}
			if today >= maxReceivesPerDay {
				return nil, &OpError{Code: proto.ErrBusy, Message: "batas salinan per hari untuk folder ini tercapai"}
			}
		}
		// Target ds@snap: nama snapshot ditentukan penerima, bukan stream.
		args = append(args, "--", ds+"@"+p.Snap)
		id, f, err := r.openRecv(args, p.Base != "", &pipeSession{recv: true, ds: ds, snap: p.Snap, prefix: rbackupPrefix(p.Job)})
		if err != nil {
			return nil, err
		}
		return FileResult{Result: proto.RBackupSession{Session: id}, File: f}, nil
	}
}

// RecvFinishOp: rbackup.recv_finish. Setelah berhasil, salinan lama
// melebihi Keep dibuang.
func (r *RemoteBackups) RecvFinishOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.RBackupRecvFinish
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		s, err := r.take(p.Session)
		if err != nil {
			return nil, err
		}
		if !s.recv {
			s.cancel()
			if !s.recv && s.snap != "" {
				r.b.discardSnap(ctx, s.ds+"@"+s.snap)
			}
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "bukan sesi penerima"}
		}
		recvErr := s.finish(p.OK)
		// Waktu terima menurut jam NAS ini (stream bisa memalsukan waktu
		// pembuatan snapshot). Dicatat untuk SETIAP snapshot yang jadi, juga
		// bila aliran dilaporkan gagal (misalnya sampah setelah akhir stream),
		// agar batas harian tidak bisa dilewati.
		now := r.b.now()
		if _, err := r.b.propsFn(ctx, s.ds+"@"+s.snap, "type"); err == nil {
			if _, err := r.b.zfs(ctx, []string{"set", receivedProp + "=" + strconv.FormatInt(now.Unix(), 10)}, s.ds+"@"+s.snap); err != nil {
				return nil, &OpError{Code: "rbackup_failed", Message: "waktu terima tidak bisa dicatat: " + clipTo(err.Error(), maxErrorText)}
			}
		}
		if recvErr != nil {
			return nil, &OpError{Code: "rbackup_failed", Message: clipTo(recvErr.Error(), maxErrorText)}
		}
		days := min(max(p.KeepDays, 1), 366)
		_ = r.b.locked(ctx, func() error {
			recv, err := r.receivedTimes(ctx, s.ds, s.prefix)
			if err != nil {
				return err
			}
			var names []string
			for name, at := range recv {
				// Yang terbaru selalu disimpan; yang lain dibuang hanya bila
				// diterima lebih dari `days` hari lalu.
				if name != s.snap && now.Sub(at) > time.Duration(days)*24*time.Hour {
					names = append(names, name)
				}
			}
			if len(names) == 0 {
				return nil
			}
			slices.Sort(names)
			_, err = r.b.zfs(ctx, []string{"destroy"}, s.ds+"@"+strings.Join(names, ","))
			return err
		})
		r.b.shares.p.logInfo("backup dari NAS lain diterima", "dataset", s.ds, "salinan", s.snap)
		return map[string]bool{"ok": true}, nil
	}
}

// ListOp: rbackup.list.
func (r *RemoteBackups) ListOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var t proto.RBackupTarget
		if err := decodeParams(raw, &t); err != nil {
			return nil, err
		}
		if err := validTarget(t, false); err != nil {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		out := proto.RBackupList{Copies: []proto.BackupCopy{}}
		if t.Job == "" {
			// Semua tugas partner: anak langsung dataset partner.
			out.Jobs = []proto.RBackupJobInfo{}
			base := t.Pool + "/sapo-backup-remote/" + t.Partner
			list, err := r.b.zfs(ctx, []string{"list", "-H", "-o", "name", "-d", "1", "-t", "filesystem"}, base)
			if err != nil {
				return out, nil // belum pernah menerima apa pun
			}
			for _, name := range strings.Split(strings.TrimSpace(list), "\n") {
				job, ok := strings.CutPrefix(name, base+"/")
				if !ok || !backupIDPattern.MatchString(job) {
					continue
				}
				jt := t
				jt.Job = job
				if exists, err := r.remoteState(ctx, jt); err != nil || !exists {
					continue
				}
				info := proto.RBackupJobInfo{Job: job, Copies: []proto.BackupCopy{}}
				if c, err := r.b.snapshots(ctx, remoteDS(jt), rbackupPrefix(job)); err == nil && c != nil {
					r.withReceived(ctx, remoteDS(jt), rbackupPrefix(job), c)
					info.Copies = c
				}
				if props, err := r.b.propsFn(ctx, remoteDS(jt), "used"); err == nil {
					info.UsedBytes, _ = strconv.ParseUint(props["used"], 10, 64)
				}
				out.Jobs = append(out.Jobs, info)
			}
			return out, nil
		}
		exists, err := r.remoteState(ctx, t)
		if err != nil || !exists {
			return out, err
		}
		if out.Copies, err = r.b.snapshots(ctx, remoteDS(t), rbackupPrefix(t.Job)); err != nil {
			return nil, err
		}
		r.withReceived(ctx, remoteDS(t), rbackupPrefix(t.Job), out.Copies)
		if out.Copies == nil {
			out.Copies = []proto.BackupCopy{}
		}
		if props, err := r.b.propsFn(ctx, remoteDS(t), "used"); err == nil {
			out.UsedBytes, _ = strconv.ParseUint(props["used"], 10, 64)
		}
		return out, nil
	}
}

// DestroyOp: rbackup.destroy (sapo-api meminta password dan konfirmasi).
// Job kosong = semua salinan partner itu.
func (r *RemoteBackups) DestroyOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var t proto.RBackupTarget
		if err := decodeParams(raw, &t); err != nil {
			return nil, err
		}
		if err := validTarget(t, false); err != nil {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		ds := t.Pool + "/sapo-backup-remote/" + t.Partner
		if t.Job != "" {
			exists, err := r.remoteState(ctx, t)
			if err != nil || !exists {
				return map[string]bool{"ok": true}, err
			}
			ds = remoteDS(t)
		} else {
			props, err := r.b.propsFn(ctx, ds, backupPropName)
			if errors.Is(err, errNoDataset) {
				return map[string]bool{"ok": true}, nil
			}
			if err != nil {
				return nil, err
			}
			if props[backupPropName] != parentMarker {
				return nil, &OpError{Code: proto.ErrConflict, Message: "dataset " + ds + " bukan milik backup Sapo"}
			}
		}
		if err := r.b.locked(ctx, func() error {
			_, err := r.b.zfs(ctx, []string{"destroy", "-r"}, ds)
			return err
		}); err != nil {
			return nil, &OpError{Code: proto.ErrPoolBusy, Message: err.Error()}
		}
		r.b.shares.p.logInfo("salinan dari NAS lain dihapus", "dataset", ds)
		return map[string]bool{"ok": true}, nil
	}
}

// RestoreOp: rbackup.restore, salinan menjadi shared folder baru di pool
// penerima (untuk memakai data saat NAS asal rusak).
func (r *RemoteBackups) RestoreOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.RBackupRestoreParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if err := validTarget(p.RBackupTarget, true); err != nil {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: err.Error()}
		}
		if !validCopyName(p.Job, p.Copy) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "salinan tidak valid"}
		}
		if !shareNamePattern.MatchString(p.Name) || slices.Contains(reservedShares, p.Name) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama folder tidak valid"}
		}
		if exists, err := r.remoteState(ctx, p.RBackupTarget); err != nil {
			return nil, err
		} else if !exists {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "salinan tidak ditemukan"}
		}
		if err := r.b.startRestoreReset(ctx, remoteMarker(p.RBackupTarget), remoteDS(p.RBackupTarget), p.Copy, p.Pool, p.Name); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	}
}

// --- Pengirim ------------------------------------------------------------------

// SendOpenOp: rbackup.send_open. Dasar inkremental = salinan terbaru di
// penerima, dan harus masih ada di sumber; bila tidak, berhenti
// (backup_diverged) tanpa menyentuh apa pun.
func (r *RemoteBackups) SendOpenOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.RBackupSendParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !poolNamePattern.MatchString(p.Pool) || !shareNamePattern.MatchString(p.Share) || !backupIDPattern.MatchString(p.Job) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "tugas tidak valid"}
		}
		job := proto.BackupJob{SrcPool: p.Pool, Share: p.Share}
		if err := r.b.checkSource(ctx, job); err != nil {
			return nil, err
		}
		prefix := rbackupPrefix(p.Job)
		var remote []string
		for _, n := range p.Remote {
			if validCopyName(p.Job, n) {
				remote = append(remote, n)
			}
		}
		slices.Sort(remote) // nama memuat waktu GMT: urutan nama = urutan waktu
		src := srcDataset(job)
		var base string
		if len(remote) > 0 {
			local, err := r.b.snapshots(ctx, src, prefix)
			if err != nil {
				return nil, err
			}
			newest := remote[len(remote)-1]
			if !slices.ContainsFunc(local, func(c proto.BackupCopy) bool { return c.Name == newest }) {
				return nil, &OpError{Code: "backup_diverged", Message: "salinan terbaru di NAS tujuan tidak ada lagi di folder ini; hapus salinan di tujuan lalu backup ulang"}
			}
			base = newest
		}
		snap := prefix + r.b.now().UTC().Format(snapTimeFormat)
		if err := r.b.locked(ctx, func() error {
			_, err := r.b.zfs(ctx, []string{"snapshot", "-o", snapAutoProp + "=backup"}, src+"@"+snap)
			return err
		}); err != nil {
			return nil, err
		}
		args := []string{"send", "-L", "-c", "--", src + "@" + snap}
		if base != "" {
			args = []string{"send", "-L", "-c", "-i", "@" + base, "--", src + "@" + snap}
		}
		id, f, err := r.open(args, false, &pipeSession{ds: src, snap: snap, prefix: prefix})
		if err != nil {
			r.b.discardSnap(ctx, src+"@"+snap)
			return nil, err
		}
		return FileResult{Result: proto.RBackupSession{Session: id, Snap: snap, Base: base}, File: f}, nil
	}
}

// SendFinishOp: rbackup.send_finish. Bila penerima menyimpan snapshot ini,
// snapshot backup lain di sumber dibuang (ini dasar berikutnya); bila tidak,
// snapshot ini yang dibuang.
func (r *RemoteBackups) SendFinishOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.RBackupSendFinish
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		s, err := r.take(p.Session)
		if err != nil {
			return nil, err
		}
		if s.recv {
			s.cancel()
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "bukan sesi pengirim"}
		}
		sendErr := s.finish(p.OK)
		if !p.RemoteHas {
			r.b.discardSnap(ctx, s.ds+"@"+s.snap)
		} else {
			_ = r.b.locked(ctx, func() error {
				old, err := r.b.snapshots(ctx, s.ds, s.prefix)
				if err != nil {
					return err
				}
				var names []string
				for _, c := range old {
					if c.Name != s.snap {
						names = append(names, c.Name)
					}
				}
				if len(names) > 0 {
					_, err = r.b.zfs(ctx, []string{"destroy"}, s.ds+"@"+strings.Join(names, ","))
				}
				return err
			})
		}
		if sendErr != nil && !p.RemoteHas {
			return nil, &OpError{Code: "rbackup_failed", Message: clipTo(sendErr.Error(), maxErrorText)}
		}
		return map[string]bool{"ok": true}, nil
	}
}

// SendCleanupOp: rbackup.send_cleanup, membuang snapshot backup tugas itu
// di sumber (tugas dihapus).
func (r *RemoteBackups) SendCleanupOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.RBackupSendParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !poolNamePattern.MatchString(p.Pool) || !shareNamePattern.MatchString(p.Share) || !backupIDPattern.MatchString(p.Job) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "tugas tidak valid"}
		}
		src := p.Pool + "/" + p.Share
		snaps, err := r.b.snapshots(ctx, src, rbackupPrefix(p.Job))
		if err != nil || len(snaps) == 0 {
			return map[string]bool{"ok": true}, nil
		}
		var names []string
		for _, c := range snaps {
			names = append(names, c.Name)
		}
		_ = r.b.locked(ctx, func() error {
			_, err := r.b.zfs(ctx, []string{"destroy"}, src+"@"+strings.Join(names, ","))
			return err
		})
		return map[string]bool{"ok": true}, nil
	}
}
