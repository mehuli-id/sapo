package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Tempat sampah shared folder (ADR 0021). File yang dihapus lewat SMB
// (vfs_recycle), File Station, dan WebDAV dipindah ke <folder>/#recycle
// dengan letak yang sama, bukan langsung hilang. Lama simpan per folder di
// properti ZFS sapo:recycle; sapod membuang isi yang lebih lama dari itu
// (menurut ctime, yaitu waktu dipindah ke tempat sampah) secara berkala.
// Menghapus sesuatu di dalam #recycle menghapusnya permanen.

const (
	recycleDir      = "#recycle"
	recyclePropName = "sapo:recycle"
)

// recycleDays membaca nilai sapo:recycle ("off", "-", atau jumlah hari).
func recycleDays(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > proto.MaxRecycleDays {
		return 0
	}
	return n
}

// recycleExcludePattern: pola nama untuk recycle:exclude (masuk smb.conf).
var recycleExcludePattern = regexp.MustCompile(`^[A-Za-z0-9.*?~$#_-]{1,64}$`)

// validRecycleExclude: hanya pola yang aman ditulis ke smb.conf.
func validRecycleExclude(list []string) []string {
	var out []string
	for _, p := range list {
		if recycleExcludePattern.MatchString(p) {
			out = append(out, p)
		}
	}
	return out
}

// inRecycle: path relatif berada di dalam (atau adalah) tempat sampah.
func inRecycle(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return first == recycleDir
}

// setRecycle menyamakan sapo:recycle dataset dengan days dan, bila
// menyala, memastikan folder #recycle ada (dibuat sapod agar izinnya
// mewarisi ACL bawaan folder, bukan milik pengguna yang pertama menghapus).
func (s *Shares) setRecycle(ctx context.Context, ds, path string, days int) error {
	if days < 0 || days > proto.MaxRecycleDays {
		return errors.New("lama simpan tempat sampah tidak valid")
	}
	want := "off"
	if days > 0 {
		want = strconv.Itoa(days)
	}
	props, err := s.zfsProps(ctx, ds, recyclePropName)
	if err != nil {
		return err
	}
	cur := props[recyclePropName]
	if recycleDays(cur) == 0 {
		cur = "off" // "-" (belum pernah diatur) atau nilai rusak
	}
	if cur != want {
		res, err := s.p.st.run(ctx, s.t.Zfs, []string{"set", recyclePropName + "=" + want}, ds)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("zfs set %s: %s", recyclePropName, firstLine(res.Stderr))
		}
	}
	if days > 0 {
		return s.mkRecycle(path)
	}
	return nil
}

// RecycleTools mengatur pembersihan tempat sampah (agent.recycle).
type RecycleTools struct {
	// Check: selang pemeriksaan dan pembersihan.
	Check time.Duration
	// EmptyTimeout: batas waktu satu "kosongkan sekarang".
	EmptyTimeout time.Duration
}

// recycleStats: hasil satu pembersihan.
type recycleStats struct {
	Bytes   uint64
	Files   int
	Removed int
	// Done: semua isi sudah diperiksa (false bila waktu habis).
	Done bool
}

// Recycle melayani recycle.* dan membersihkan tempat sampah berkala.
type Recycle struct {
	shares *Shares
	t      RecycleTools
	// mu: satu pembersihan pada satu waktu.
	mu      sync.Mutex
	usageMu sync.Mutex
	usage   map[string]proto.RecycleUsage
	now     func() time.Time
	// clean membuang isi #recycle di root yang ctime-nya sebelum cutoff
	// (diganti di test).
	clean func(root string, cutoff, deadline time.Time) (recycleStats, error)
	// listFn: daftar folder (diganti di test).
	listFn func(ctx context.Context) ([]recycleShare, error)
}

// NewRecycle menyiapkan pembersihan tempat sampah folder s.
func NewRecycle(s *Shares, t RecycleTools) *Recycle {
	if t.Check == 0 {
		t.Check = time.Hour
	}
	if t.EmptyTimeout == 0 {
		t.EmptyTimeout = 4 * time.Minute
	}
	r := &Recycle{shares: s, t: t, usage: map[string]proto.RecycleUsage{}, now: time.Now, clean: cleanRecycle}
	r.listFn = r.list
	return r
}

// recycleShare: folder yang tempat sampahnya diperiksa.
type recycleShare struct {
	pool, share, root string
	days              int
}

// list: shared folder Sapo yang terpasang di tempatnya.
func (r *Recycle) list(ctx context.Context) ([]recycleShare, error) {
	sh := r.shares
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := sh.p.st.run(ctx, sh.t.Zfs, []string{"list", "-H", "-p", "-t", "filesystem", "-o", "name," + sharePropName + "," + recyclePropName + ",mounted,mountpoint"})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, errors.New(clipTo(firstLine(res.Stderr), maxErrorText))
	}
	return parseRecycleList(res.Stdout, sh.t.MountBase), nil
}

// parseRecycleList: keluaran zfs list name,sapo:share,sapo:recycle,
// mounted,mountpoint → shared folder Sapo (<pool>/<nama> bertanda
// sapo:share=<nama>) yang terpasang tepat di <mountBase>/<pool>/<nama>.
func parseRecycleList(stdout []byte, mountBase string) []recycleShare {
	var out []recycleShare
	for _, line := range strings.Split(string(stdout), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 5 {
			continue
		}
		pool, name, ok := strings.Cut(f[0], "/")
		root := filepath.Join(mountBase, pool, name)
		if !ok || strings.Contains(name, "/") || f[1] != name || f[3] != "yes" || f[4] != root || !shareNamePattern.MatchString(name) {
			continue
		}
		out = append(out, recycleShare{pool: pool, share: name, root: root, days: recycleDays(f[2])})
	}
	return out
}

// Tick membersihkan semua tempat sampah dan mencatat isinya.
func (r *Recycle) Tick(ctx context.Context) {
	list, err := r.listFn(ctx)
	if err != nil {
		if !errors.Is(err, errToolMissing) {
			r.shares.p.logWarn("tempat sampah: daftar folder gagal dibaca", "err", err)
		}
		return
	}
	usage := map[string]proto.RecycleUsage{}
	for _, sh := range list {
		if ctx.Err() != nil {
			return
		}
		now := r.now()
		// Mati: isi tidak dibuang (hanya dihitung).
		var cutoff time.Time
		if sh.days > 0 {
			cutoff = now.Add(-time.Duration(sh.days) * 24 * time.Hour)
		}
		r.mu.Lock()
		st, err := r.clean(sh.root, cutoff, now.Add(10*time.Minute))
		r.mu.Unlock()
		if err != nil {
			r.shares.p.logWarn("tempat sampah gagal dibersihkan", "folder", sh.share, "err", err)
			continue
		}
		if st.Removed > 0 {
			r.shares.p.logInfo("tempat sampah dibersihkan", "folder", sh.share, "dibuang", st.Removed)
		}
		usage[sh.pool+"/"+sh.share] = proto.RecycleUsage{Pool: sh.pool, Share: sh.share, Days: sh.days, Bytes: st.Bytes, Files: st.Files, Checked: now.UTC()}
	}
	r.usageMu.Lock()
	r.usage = usage
	r.usageMu.Unlock()
}

// Run memeriksa tempat sampah setiap t.Check sampai ctx selesai.
func (r *Recycle) Run(ctx context.Context) {
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.t.Check):
		}
	}
}

// StatusOp: recycle.status (isi tempat sampah saat terakhir diperiksa).
func (r *Recycle) StatusOp() Handler {
	return func(context.Context, json.RawMessage) (any, error) {
		r.usageMu.Lock()
		out := make([]proto.RecycleUsage, 0, len(r.usage))
		for _, u := range r.usage {
			out = append(out, u)
		}
		r.usageMu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].Pool+"/"+out[i].Share < out[j].Pool+"/"+out[j].Share })
		return out, nil
	}
}

// EmptyOp: recycle.empty. Seluruh isi tempat sampah satu folder dihapus
// permanen (sapo-api meminta password admin dan mencatat audit).
func (r *Recycle) EmptyOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.RecycleTarget
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !poolNamePattern.MatchString(p.Pool) || !shareNamePattern.MatchString(p.Share) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "folder tidak valid"}
		}
		list, err := r.listFn(ctx)
		if err != nil {
			return nil, err
		}
		var sh *recycleShare
		for i := range list {
			if list[i].pool == p.Pool && list[i].share == p.Share {
				sh = &list[i]
			}
		}
		if sh == nil {
			return nil, &OpError{Code: proto.ErrNotFound, Message: "folder tidak ditemukan atau belum terpasang"}
		}
		if !r.mu.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "tempat sampah sedang dibersihkan; coba lagi sebentar"}
		}
		now := r.now()
		st, err := r.clean(sh.root, now.Add(100*365*24*time.Hour), now.Add(r.t.EmptyTimeout))
		r.mu.Unlock()
		if err != nil {
			return nil, err
		}
		r.usageMu.Lock()
		r.usage[sh.pool+"/"+sh.share] = proto.RecycleUsage{Pool: sh.pool, Share: sh.share, Days: sh.days, Bytes: st.Bytes, Files: st.Files, Checked: now.UTC()}
		r.usageMu.Unlock()
		r.shares.p.logInfo("tempat sampah dikosongkan", "folder", sh.share, "dibuang", st.Removed, "selesai", st.Done)
		return proto.RecycleEmptyResult{Removed: st.Removed, Done: st.Done}, nil
	}
}
