package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

const maxRBackupSessions = 4

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
	for id, s := range r.sessions {
		if now.After(s.expires) {
			s.cancel()
			delete(r.sessions, id)
		}
	}
	if len(r.sessions) >= maxRBackupSessions {
		return "", nil, &OpError{Code: proto.ErrBusy, Message: "terlalu banyak backup jarak jauh yang berjalan"}
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
	out, err := r.b.zfs(ctx, []string{"list", "-H", "-o", "name", "-r", "-d", "2"}, pool+"/sapo-backup-remote")
	if err != nil {
		return nil // tidak ada salinan dari NAS lain
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
func (r *RemoteBackups) ensureRemoteParents(ctx context.Context, t proto.RBackupTarget) error {
	for _, ds := range []string{t.Pool + "/sapo-backup-remote", t.Pool + "/sapo-backup-remote/" + t.Partner} {
		props, err := r.b.propsFn(ctx, ds, backupPropName, sharePropName)
		if errors.Is(err, errNoDataset) {
			if _, err := r.b.zfs(ctx, []string{"create", "-o", backupPropName + "=" + parentMarker, "-o", "canmount=off", "-o", "devices=off", "-o", "setuid=off"}, ds); err != nil {
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
		if err := r.ensureRemoteParents(ctx, p.RBackupTarget); err != nil {
			return nil, err
		}
		exists, err := r.remoteState(ctx, p.RBackupTarget)
		if err != nil {
			return nil, err
		}
		ds := remoteDS(p.RBackupTarget)
		var args []string
		switch {
		case p.Base == "" && exists:
			return nil, &OpError{Code: "backup_diverged", Message: "salinan tugas ini sudah ada di penerima; kirim inkremental dari salinan terbarunya"}
		case p.Base == "":
			args = []string{"receive", "-u", "-o", "canmount=noauto", "-o", "readonly=on", "-o", backupPropName + "=" + remoteMarker(p.RBackupTarget), "--", ds}
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
			args = []string{"receive", "-u", "--", ds}
		}
		id, f, err := r.open(args, true, &pipeSession{recv: true, ds: ds, snap: p.Snap, prefix: rbackupPrefix(p.Job)})
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
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "bukan sesi penerima"}
		}
		if err := s.finish(p.OK); err != nil {
			return nil, &OpError{Code: "rbackup_failed", Message: clipTo(err.Error(), maxErrorText)}
		}
		keep := min(max(p.Keep, 1), 366)
		_ = r.b.locked(ctx, func() error {
			have, err := r.b.snapshots(ctx, s.ds, s.prefix)
			if err != nil || len(have) <= keep {
				return err
			}
			var names []string
			for _, c := range have[:len(have)-keep] {
				names = append(names, c.Name)
			}
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
		if err := r.b.startRestore(ctx, remoteMarker(p.RBackupTarget), remoteDS(p.RBackupTarget), p.Copy, p.Pool, p.Name); err != nil {
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
