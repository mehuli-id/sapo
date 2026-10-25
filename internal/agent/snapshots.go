package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Snapshot ZFS terjadwal (ADR 0014). Jadwal setiap dataset (shared folder
// atau folder pribadi) disimpan di properti ZFS sapo:snap di dataset itu
// sendiri, jadi ikut terbawa bersama datanya. sapod memeriksa jadwal setiap
// menit, mengambil snapshot yang sudah waktunya, dan membuang snapshot
// otomatis yang melebihi jumlah simpan. Snapshot manual dan snapshot yang
// bukan buatan Sapo tidak pernah dihapus otomatis.
//
// Nama snapshot: sapo-<jenis>_GMT-YYYY.MM.DD-HH.MM.SS (waktu UTC), format
// yang dikenali "Previous Versions" Windows lewat Samba (shadow_copy2).

const snapPropName = "sapo:snap"

var snapNamePattern = regexp.MustCompile(`^sapo-(hourly|daily|weekly|manual)_GMT-(\d{4}\.\d{2}\.\d{2}-\d{2}\.\d{2}\.\d{2})$`)

const snapTimeFormat = "2006.01.02-15.04.05"

func snapName(kind string, t time.Time) string {
	return "sapo-" + kind + "_GMT-" + t.UTC().Format(snapTimeFormat)
}

// encodeSnapPolicy: "h24.d14.w8.t3", atau "off".
func encodeSnapPolicy(p *proto.SnapshotPolicy) string {
	if p == nil || (p.Hourly == 0 && p.Daily == 0 && p.Weekly == 0) {
		return "off"
	}
	return fmt.Sprintf("h%d.d%d.w%d.t%d", p.Hourly, p.Daily, p.Weekly, p.Hour)
}

func decodeSnapPolicy(v string) (proto.SnapshotPolicy, bool) {
	var p proto.SnapshotPolicy
	if _, err := fmt.Sscanf(v, "h%d.d%d.w%d.t%d", &p.Hourly, &p.Daily, &p.Weekly, &p.Hour); err != nil || !p.Valid() || encodeSnapPolicy(&p) != v {
		return proto.SnapshotPolicy{}, false
	}
	return p, true
}

// setSnapPolicy menyamakan properti sapo:snap dataset dengan jadwal p.
func (s *Shares) setSnapPolicy(ctx context.Context, ds string, p *proto.SnapshotPolicy) error {
	if p != nil && !p.Valid() {
		return errors.New("jadwal snapshot tidak valid")
	}
	want := encodeSnapPolicy(p)
	props, err := s.zfsProps(ctx, ds, snapPropName)
	if err != nil {
		return err
	}
	cur := props[snapPropName]
	if cur == "-" || cur == "" {
		cur = "off"
	}
	if cur == want {
		return nil
	}
	res, err := s.p.st.run(ctx, s.t.Zfs, []string{"set", snapPropName + "=" + want}, ds)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("zfs set %s: %s", snapPropName, firstLine(res.Stderr))
	}
	return nil
}

// Snapshots melayani snapshots.* dan menjalankan jadwal.
type Snapshots struct {
	shares *Shares
	// now dan location diganti di test.
	now      func() time.Time
	location func() *time.Location
}

// NewSnapshots menyiapkan snapshot di atas shared folder (zfs, kunci pool).
func NewSnapshots(shares *Shares) *Snapshots {
	return &Snapshots{shares: shares, now: time.Now, location: localZone}
}

// localZone membaca zona waktu sistem setiap kali (bisa diubah dari Control
// Panel tanpa me-restart sapod).
func localZone() *time.Location {
	data, err := os.ReadFile("/etc/localtime")
	if err != nil {
		return time.Local
	}
	loc, err := time.LoadLocationFromTZData("Local", data)
	if err != nil {
		return time.Local
	}
	return loc
}

// Run menjalankan jadwal setiap menit sampai ctx selesai.
func (sn *Snapshots) Run(ctx context.Context) {
	for {
		sn.Tick(ctx)
		next := sn.now().Truncate(time.Minute).Add(time.Minute + 5*time.Second)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
	}
}

type snapEntry struct {
	name    string
	kind    string
	created time.Time
	used    uint64
}

// Tick mengambil snapshot yang sudah waktunya dan membuang yang lama. Bila
// operasi pool lain sedang berjalan, dilewati sampai menit berikutnya.
func (sn *Snapshots) Tick(ctx context.Context) {
	sh := sn.shares
	if !sh.p.write.TryLock() {
		return
	}
	defer sh.p.write.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := sh.p.st.run(ctx, sh.t.Zfs, []string{"list", "-H", "-p", "-t", "filesystem", "-o", "name," + snapPropName + "," + sharePropName + "," + homePropName})
	if err != nil || res.ExitCode != 0 {
		return
	}
	type dsPolicy struct {
		ds string
		p  proto.SnapshotPolicy
	}
	var due []dsPolicy
	sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 4 || !sapoDataset(f[0], f[2], f[3]) {
			continue
		}
		if p, ok := decodeSnapPolicy(f[1]); ok {
			due = append(due, dsPolicy{f[0], p})
		}
	}
	if len(due) == 0 {
		return
	}
	snaps, err := sn.listAll(ctx)
	if err != nil {
		return
	}
	now := sn.now()
	loc := sn.location()
	for _, d := range due {
		sn.apply(ctx, d.ds, d.p, snaps[d.ds], now, loc)
	}
}

// sapoDataset: <pool>/<folder> bertanda sapo:share, atau
// <pool>/sapo-home/<nama> bertanda sapo:home. Dataset lain tidak pernah
// disentuh, walau propertinya diset dari terminal.
func sapoDataset(ds, share, home string) bool {
	parts := strings.Split(ds, "/")
	switch {
	case len(parts) == 2 && parts[1] == share && shareNamePattern.MatchString(share):
		return true
	case len(parts) == 3 && parts[1] == homesDataset && parts[2] == home && accountNamePattern.MatchString(home):
		return true
	}
	return false
}

// listAll: snapshot buatan Sapo per dataset.
func (sn *Snapshots) listAll(ctx context.Context) (map[string][]snapEntry, error) {
	sh := sn.shares
	res, err := sh.p.st.run(ctx, sh.t.Zfs, []string{"list", "-H", "-p", "-t", "snapshot", "-o", "name,creation,used"})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("zfs list: %s", firstLine(res.Stderr))
	}
	out := map[string][]snapEntry{}
	sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 3 {
			continue
		}
		ds, name, ok := strings.Cut(f[0], "@")
		m := snapNamePattern.FindStringSubmatch(name)
		if !ok || m == nil {
			continue
		}
		sec, _ := strconv.ParseInt(f[1], 10, 64)
		used, _ := strconv.ParseUint(f[2], 10, 64)
		out[ds] = append(out[ds], snapEntry{name: name, kind: m[1], created: time.Unix(sec, 0), used: used})
	}
	return out, nil
}

// periodStart: awal periode jenis kind yang sedang berjalan (waktu lokal):
// awal jam ini; hari ini jam p.Hour (atau kemarin bila belum sampai); Senin
// jam p.Hour minggu ini (atau minggu lalu).
func periodStart(kind string, p proto.SnapshotPolicy, now time.Time, loc *time.Location) time.Time {
	l := now.In(loc)
	switch kind {
	case "hourly":
		return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, loc)
	case "daily":
		t := time.Date(l.Year(), l.Month(), l.Day(), p.Hour, 0, 0, 0, loc)
		if t.After(l) {
			t = t.AddDate(0, 0, -1)
		}
		return t
	default: // weekly
		back := (int(l.Weekday()) + 6) % 7 // hari sejak Senin
		t := time.Date(l.Year(), l.Month(), l.Day()-back, p.Hour, 0, 0, 0, loc)
		if t.After(l) {
			t = t.AddDate(0, 0, -7)
		}
		return t
	}
}

func (sn *Snapshots) apply(ctx context.Context, ds string, p proto.SnapshotPolicy, have []snapEntry, now time.Time, loc *time.Location) {
	sh := sn.shares
	for _, k := range []struct {
		kind string
		keep int
	}{{"hourly", p.Hourly}, {"daily", p.Daily}, {"weekly", p.Weekly}} {
		if k.keep == 0 {
			// Jenis yang dimatikan: snapshot lamanya dibiarkan.
			continue
		}
		var mine []snapEntry
		for _, e := range have {
			if e.kind == k.kind {
				mine = append(mine, e)
			}
		}
		start := periodStart(k.kind, p, now, loc)
		taken := false
		for _, e := range mine {
			if !e.created.Before(start) {
				taken = true
			}
		}
		if !taken {
			name := snapName(k.kind, now)
			res, err := sh.p.st.run(ctx, sh.t.Zfs, []string{"snapshot"}, ds+"@"+name)
			if err != nil || res.ExitCode != 0 {
				sh.p.logWarn("snapshot gagal diambil", "dataset", ds, "jenis", k.kind, "err", err)
				continue
			}
			mine = append(mine, snapEntry{name: name, kind: k.kind, created: now})
		}
		// Terbaru dulu; yang melebihi jumlah simpan dibuang.
		sort.Slice(mine, func(i, j int) bool { return mine[i].created.After(mine[j].created) })
		for _, e := range mine[min(len(mine), k.keep):] {
			res, err := sh.p.st.run(ctx, sh.t.Zfs, []string{"destroy"}, ds+"@"+e.name)
			if err != nil || res.ExitCode != 0 {
				sh.p.logWarn("snapshot lama gagal dibuang", "snapshot", ds+"@"+e.name, "err", err)
			}
		}
	}
}

// --- Handler -----------------------------------------------------------------

// dataset memeriksa target: shared folder atau folder pribadi buatan Sapo.
func (sn *Snapshots) dataset(ctx context.Context, t proto.SnapshotTarget) (string, error) {
	sh := sn.shares
	invalid := &OpError{Code: proto.ErrInvalidParams, Message: "folder tidak valid"}
	if !poolNamePattern.MatchString(t.Pool) || (t.Share == "") == (t.User == "") {
		return "", invalid
	}
	ds, prop, want := t.Pool+"/"+t.Share, sharePropName, t.Share
	if t.Share == "" {
		if !accountNamePattern.MatchString(t.User) {
			return "", invalid
		}
		ds, prop, want = t.Pool+"/"+homesDataset+"/"+t.User, homePropName, t.User
	} else if !shareNamePattern.MatchString(t.Share) {
		return "", invalid
	}
	props, err := sh.zfsProps(ctx, ds, "type", prop)
	if errors.Is(err, errNoDataset) || (err == nil && (props["type"] != "filesystem" || props[prop] != want)) {
		return "", &OpError{Code: proto.ErrNotFound, Message: "folder tidak ditemukan"}
	}
	return ds, err
}

// ListOp mengembalikan handler snapshots.list.
func (sn *Snapshots) ListOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var t proto.SnapshotTarget
		if err := decodeParams(raw, &t); err != nil {
			return nil, err
		}
		ds, err := sn.dataset(ctx, t)
		if err != nil {
			return nil, err
		}
		sh := sn.shares
		res, err := sh.p.st.run(ctx, sh.t.Zfs, []string{"list", "-H", "-p", "-t", "snapshot", "-d", "1", "-o", "name,creation,used"}, ds)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("zfs list: %s", firstLine(res.Stderr))
		}
		out := proto.SnapshotsListResult{Snapshots: []proto.SnapshotInfo{}}
		sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
		for sc.Scan() {
			f := strings.Split(sc.Text(), "\t")
			if len(f) != 3 {
				continue
			}
			_, name, _ := strings.Cut(f[0], "@")
			m := snapNamePattern.FindStringSubmatch(name)
			if m == nil {
				continue
			}
			sec, _ := strconv.ParseInt(f[1], 10, 64)
			used, _ := strconv.ParseUint(f[2], 10, 64)
			out.Snapshots = append(out.Snapshots, proto.SnapshotInfo{Name: name, Kind: m[1], Created: time.Unix(sec, 0).UTC(), UsedBytes: used})
		}
		sort.Slice(out.Snapshots, func(i, j int) bool { return out.Snapshots[i].Created.After(out.Snapshots[j].Created) })
		if props, err := sh.zfsProps(ctx, ds, "usedbysnapshots"); err == nil {
			out.UsedBySnapshots, _ = strconv.ParseUint(props["usedbysnapshots"], 10, 64)
		}
		return out, nil
	}
}

// CreateOp mengembalikan handler snapshots.create: snapshot manual (tidak
// pernah dihapus otomatis).
func (sn *Snapshots) CreateOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var t proto.SnapshotTarget
		if err := decodeParams(raw, &t); err != nil {
			return nil, err
		}
		sh := sn.shares
		if !sh.p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer sh.p.write.Unlock()
		ds, err := sn.dataset(ctx, t)
		if err != nil {
			return nil, err
		}
		name := snapName("manual", sn.now())
		res, err := sh.p.st.run(ctx, sh.t.Zfs, []string{"snapshot"}, ds+"@"+name)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			if strings.Contains(string(res.Stderr), "exists") {
				return nil, &OpError{Code: proto.ErrConflict, Message: "snapshot baru saja diambil; coba lagi sebentar"}
			}
			return nil, fmt.Errorf("zfs snapshot: %s", firstLine(res.Stderr))
		}
		sh.p.logInfo("snapshot manual diambil", "snapshot", ds+"@"+name)
		return map[string]string{"name": name}, nil
	}
}

// DeleteOp mengembalikan handler snapshots.delete: satu snapshot buatan
// Sapo. Destruktif; sapo-api meminta password admin dan mencatatnya.
func (sn *Snapshots) DeleteOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.SnapshotParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !snapNamePattern.MatchString(p.Name) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama snapshot tidak valid"}
		}
		sh := sn.shares
		if !sh.p.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer sh.p.write.Unlock()
		ds, err := sn.dataset(ctx, p.SnapshotTarget)
		if err != nil {
			return nil, err
		}
		// Tanpa -r/-R: hanya snapshot ini; bila masih dipakai (clone),
		// ditolak zfs.
		res, err := sh.p.st.run(ctx, sh.t.Zfs, []string{"destroy"}, ds+"@"+p.Name)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			msg := firstLine(res.Stderr)
			if strings.Contains(msg, "could not find") || strings.Contains(msg, "does not exist") {
				return nil, &OpError{Code: proto.ErrNotFound, Message: "snapshot tidak ditemukan"}
			}
			return nil, &OpError{Code: proto.ErrPoolBusy, Message: clipTo(msg, maxErrorText)}
		}
		sh.p.logInfo("snapshot dihapus", "snapshot", ds+"@"+p.Name)
		return map[string]bool{"ok": true}, nil
	}
}
