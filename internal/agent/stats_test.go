package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestStats(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"net/eth0/device", "net/docker0", "block/sda/device", "block/zram0/device"} {
		_ = os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	write := func(cpuBusy, cpuIdle, iowait, rx, tx, rd, wr int) {
		_ = os.WriteFile(filepath.Join(dir, "stat"), []byte("cpu  "+itoa(cpuBusy)+" 0 0 "+itoa(cpuIdle)+" "+itoa(iowait)+" 0 0 0 0 0\ncpu0 1 2 3 4 5 6 7 8\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "meminfo"), []byte("MemTotal:  1000 kB\nMemAvailable:  250 kB\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "dev"), []byte("Inter-|   Receive\n face |bytes\n    lo: 999 0 0 0 0 0 0 0 999 0 0 0 0 0 0 0\n  eth0: "+itoa(rx)+" 0 0 0 0 0 0 0 "+itoa(tx)+" 0 0 0 0 0 0 0\ndocker0: 5 0 0 0 0 0 0 0 5 0 0 0 0 0 0 0\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "diskstats"), []byte("   8 0 sda 1 0 "+itoa(rd)+" 0 1 0 "+itoa(wr)+" 0 0 0 0\n   8 1 sda1 1 0 99 0 1 0 99 0 0 0 0\n 252 0 zram0 1 0 50 0 1 0 50 0 0 0 0\n"), 0o644)
	}
	write(100, 900, 0, 1000, 2000, 0, 0)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	s := NewStats(StatsTools{
		ProcStat: filepath.Join(dir, "stat"), MemInfo: filepath.Join(dir, "meminfo"), NetDev: filepath.Join(dir, "dev"),
		DiskStats: filepath.Join(dir, "diskstats"), SysNet: filepath.Join(dir, "net"), SysBlock: filepath.Join(dir, "block"),
		Interval: time.Hour, History: 2, IdleStop: time.Hour, IgnoreDevices: []string{"zram*"},
	})
	s.now = func() time.Time { return now }
	get := func(after uint64) proto.SystemStats {
		raw, _ := json.Marshal(proto.StatsParams{After: after})
		out, err := s.StatsOp()(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		return out.(proto.SystemStats)
	}
	if st := get(0); len(st.Samples) != 0 {
		t.Fatalf("sampel pertama hanya titik awal: %+v", st)
	}
	// 2 detik kemudian: CPU 40% sibuk (20% menunggu disk), eth0 1000 B/s
	// masuk, sda 512 B/s dibaca.
	write(300, 1100, 100, 3000, 2000, 2, 4)
	now = now.Add(2 * time.Second)
	s.mu.Lock()
	s.sampleLocked()
	s.mu.Unlock()
	st := get(0)
	if len(st.Samples) != 1 {
		t.Fatalf("sampel = %+v", st)
	}
	smp := st.Samples[0]
	if smp.CPUPercent != 40 || smp.IOWaitPercent != 20 || smp.MemoryUsedBytes != 750*1024 || smp.MemoryTotalBytes != 1000*1024 {
		t.Fatalf("sampel = %+v", smp)
	}
	if len(smp.Net) != 1 || smp.Net[0] != (proto.StatsRate{Name: "eth0", In: 1000, Out: 0}) {
		t.Fatalf("jaringan (hanya antarmuka fisik) = %+v", smp.Net)
	}
	if len(smp.Disk) != 1 || smp.Disk[0] != (proto.StatsRate{Name: "sda", In: 512, Out: 1024}) {
		t.Fatalf("disk (tanpa partisi dan zram) = %+v", smp.Disk)
	}
	// Hanya sampel baru, dan riwayat dibatasi History.
	for i := 0; i < 3; i++ {
		now = now.Add(2 * time.Second)
		s.mu.Lock()
		s.sampleLocked()
		s.mu.Unlock()
	}
	st = get(0)
	if len(st.Samples) != 2 || st.Samples[1].Seq != 4 {
		t.Fatalf("riwayat = %+v", st.Samples)
	}
	if st := get(3); len(st.Samples) != 1 {
		t.Fatalf("sampel setelah nomor 3 = %d", len(st.Samples))
	}
	// iowait yang mundur tidak menjadi lonjakan 100%.
	write(400, 1200, 50, 3000, 2000, 2, 4)
	now = now.Add(2 * time.Second)
	s.mu.Lock()
	s.sampleLocked()
	s.mu.Unlock()
	if last := get(4).Samples[0]; last.IOWaitPercent != 0 || last.CPUPercent > 100 {
		t.Fatalf("iowait mundur = %+v", last)
	}
	// Jawaban dibatasi; sisanya diambil dengan nomor terakhir.
	s.t.History = 500
	for i := 0; i < proto.MaxStatsSamples+10; i++ {
		now = now.Add(2 * time.Second)
		s.mu.Lock()
		s.sampleLocked()
		s.mu.Unlock()
	}
	first := get(0).Samples
	if len(first) != proto.MaxStatsSamples || len(get(first[len(first)-1].Seq).Samples) == 0 {
		t.Fatalf("halaman = %d", len(first))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
