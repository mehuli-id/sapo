package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// StatsTools mengatur system.stats (agent.stats di sapo.yaml).
type StatsTools struct {
	// File /proc dan /sys yang dibaca (diganti di test).
	ProcStat, MemInfo, NetDev, DiskStats string
	SysNet, SysBlock                     string
	// Interval pengambilan sampel dan jumlah sampel yang disimpan.
	Interval time.Duration
	History  int
	// IdleStop: pengambilan sampel berhenti bila tidak ada yang membaca
	// selama ini (Resource Monitor ditutup).
	IdleStop time.Duration
	// IgnoreDevices: pola nama disk yang tidak ditampilkan (zram*, loop*).
	IgnoreDevices []string
}

// Stats mengambil sampel pemakaian sistem di latar belakang, hanya selama
// ada yang membaca.
type Stats struct {
	t StatsTools

	mu       sync.Mutex
	running  bool
	lastRead time.Time
	samples  []proto.StatsSample
	prev     *rawStats
	now      func() time.Time
}

// rawStats adalah penghitung kumulatif dari kernel.
type rawStats struct {
	at        time.Time
	cpuTotal  uint64
	cpuIdle   uint64
	cpuIOWait uint64
	net, disk map[string][2]uint64
	memUsed   uint64
	memTotal  uint64
}

// NewStats menyiapkan Resource Monitor.
func NewStats(t StatsTools) *Stats {
	return &Stats{t: t, now: time.Now}
}

// StatsOp mengembalikan handler system.stats.
func (s *Stats) StatsOp() Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var req proto.StatsParams
		if len(raw) > 0 && string(raw) != "null" {
			if err := decodeParams(raw, &req); err != nil {
				return nil, err
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lastRead = s.now()
		if !s.running {
			s.running = true
			s.prev = nil
			s.samples = nil
			s.sampleLocked()
			go s.loop()
		}
		out := proto.SystemStats{IntervalSeconds: s.t.Interval.Seconds(), CPUCores: runtime.NumCPU(), Samples: []proto.StatsSample{}}
		for _, smp := range s.samples {
			if smp.Time.After(req.Since) {
				out.Samples = append(out.Samples, smp)
			}
		}
		return out, nil
	}
}

func (s *Stats) loop() {
	tick := time.NewTicker(s.t.Interval)
	defer tick.Stop()
	for range tick.C {
		s.mu.Lock()
		if s.now().Sub(s.lastRead) > s.t.IdleStop {
			s.running, s.samples, s.prev = false, nil, nil
			s.mu.Unlock()
			return
		}
		s.sampleLocked()
		s.mu.Unlock()
	}
}

// sampleLocked membaca penghitung kernel dan menyimpan laju sejak sampel
// sebelumnya.
func (s *Stats) sampleLocked() {
	cur := s.read()
	if p := s.prev; p != nil {
		secs := cur.at.Sub(p.at).Seconds()
		if secs > 0 {
			smp := proto.StatsSample{Time: cur.at.UTC(), MemoryUsedBytes: cur.memUsed, MemoryTotalBytes: cur.memTotal,
				Net: rates(p.net, cur.net, secs, 1), Disk: rates(p.disk, cur.disk, secs, 512)}
			if dt := float64(cur.cpuTotal - p.cpuTotal); cur.cpuTotal > p.cpuTotal {
				smp.CPUPercent = clampPct(100 * (dt - float64(cur.cpuIdle-p.cpuIdle)) / dt)
				smp.IOWaitPercent = clampPct(100 * float64(cur.cpuIOWait-p.cpuIOWait) / dt)
			}
			s.samples = append(s.samples, smp)
			if len(s.samples) > s.t.History {
				s.samples = slices.Delete(s.samples, 0, len(s.samples)-s.t.History)
			}
		}
	}
	s.prev = &cur
}

func clampPct(v float64) float64 {
	return max(0, min(100, v))
}

// rates menghitung laju per detik; unit adalah ukuran satu hitungan (512
// untuk sektor disk). Penghitung yang mundur (perangkat baru) dilewati.
func rates(prev, cur map[string][2]uint64, secs float64, unit float64) []proto.StatsRate {
	out := []proto.StatsRate{}
	for name, c := range cur {
		p, ok := prev[name]
		if !ok || c[0] < p[0] || c[1] < p[1] {
			continue
		}
		out = append(out, proto.StatsRate{Name: name, In: float64(c[0]-p[0]) * unit / secs, Out: float64(c[1]-p[1]) * unit / secs})
	}
	slices.SortFunc(out, func(a, b proto.StatsRate) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (s *Stats) read() rawStats {
	r := rawStats{at: s.now(), net: map[string][2]uint64{}, disk: map[string][2]uint64{}}
	if data, err := os.ReadFile(s.t.ProcStat); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 6 || f[0] != "cpu" {
				continue
			}
			// user nice system idle iowait irq softirq steal (guest
			// sudah termasuk di user).
			for i, v := range f[1:] {
				if i >= 8 {
					break
				}
				n, _ := strconv.ParseUint(v, 10, 64)
				r.cpuTotal += n
				switch i {
				case 3:
					r.cpuIdle += n
				case 4:
					r.cpuIdle += n
					r.cpuIOWait = n
				}
			}
			break
		}
	}
	if f, err := os.Open(s.t.MemInfo); err == nil {
		m := parseMemInfo(f)
		f.Close()
		r.memTotal = m.TotalBytes
		if m.TotalBytes > m.AvailableBytes {
			r.memUsed = m.TotalBytes - m.AvailableBytes
		}
	}
	if data, err := os.ReadFile(s.t.NetDev); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			name, rest, ok := strings.Cut(sc.Text(), ":")
			name = strings.TrimSpace(name)
			f := strings.Fields(rest)
			// Hanya antarmuka fisik (bukan lo, bridge, atau veth container).
			if !ok || len(f) < 9 || !ifacePattern.MatchString(name) {
				continue
			}
			if _, err := os.Stat(filepath.Join(s.t.SysNet, name, "device")); err != nil {
				continue
			}
			rx, _ := strconv.ParseUint(f[0], 10, 64)
			tx, _ := strconv.ParseUint(f[8], 10, 64)
			r.net[name] = [2]uint64{rx, tx}
		}
	}
	if data, err := os.ReadFile(s.t.DiskStats); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 10 || s.ignoredDisk(f[2]) {
				continue
			}
			// Hanya disk utuh (ada di /sys/block dengan perangkat), bukan
			// partisi.
			if _, err := os.Stat(filepath.Join(s.t.SysBlock, f[2], "device")); err != nil {
				continue
			}
			rd, _ := strconv.ParseUint(f[5], 10, 64)
			wr, _ := strconv.ParseUint(f[9], 10, 64)
			r.disk[f[2]] = [2]uint64{rd, wr}
		}
	}
	return r
}

func (s *Stats) ignoredDisk(name string) bool {
	if !ifacePattern.MatchString(name) {
		return true
	}
	for _, p := range s.t.IgnoreDevices {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}
