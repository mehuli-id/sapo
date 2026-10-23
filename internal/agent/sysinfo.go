package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/mehuli-id/sapo/internal/proto"
)

// ProcFS menunjuk lokasi file sistem yang dibaca system.info. Bisa diganti
// di test agar tidak bergantung pada mesin yang menjalankan test.
type ProcFS struct {
	OSRelease string // /etc/os-release
	Hostname  string // /proc/sys/kernel/hostname
	Kernel    string // /proc/sys/kernel/osrelease
	Uptime    string // /proc/uptime
	CPUInfo   string // /proc/cpuinfo
	MemInfo   string // /proc/meminfo
	LoadAvg   string // /proc/loadavg
}

// DefaultProcFS adalah lokasi standar di Linux.
var DefaultProcFS = ProcFS{
	OSRelease: "/etc/os-release",
	Hostname:  "/proc/sys/kernel/hostname",
	Kernel:    "/proc/sys/kernel/osrelease",
	Uptime:    "/proc/uptime",
	CPUInfo:   "/proc/cpuinfo",
	MemInfo:   "/proc/meminfo",
	LoadAvg:   "/proc/loadavg",
}

// SystemInfoOp mengembalikan handler system.info. Operasi ini hanya membaca.
func SystemInfoOp(fsys ProcFS, version string) Handler {
	return func(_ context.Context, _ json.RawMessage) (any, error) {
		return ReadSystemInfo(fsys, version), nil
	}
}

// ReadSystemInfo mengumpulkan info sistem. Nilai yang gagal dibaca dibiarkan kosong.
func ReadSystemInfo(fsys ProcFS, version string) proto.SystemInfo {
	info := proto.SystemInfo{Arch: runtime.GOARCH, SapoVersion: version}
	info.Hostname = readTrim(fsys.Hostname)
	if info.Hostname == "" {
		info.Hostname, _ = os.Hostname()
	}
	info.Kernel = readTrim(fsys.Kernel)
	if f, err := os.Open(fsys.OSRelease); err == nil {
		info.OS = parseOSRelease(f)
		f.Close()
	}
	if f, err := os.Open(fsys.CPUInfo); err == nil {
		info.CPU = parseCPUInfo(f)
		f.Close()
	}
	if info.CPU.Cores == 0 {
		info.CPU.Cores = runtime.NumCPU()
	}
	if f, err := os.Open(fsys.MemInfo); err == nil {
		info.Memory = parseMemInfo(f)
		f.Close()
	}
	if fields := strings.Fields(readTrim(fsys.Uptime)); len(fields) > 0 {
		info.UptimeSeconds, _ = strconv.ParseFloat(fields[0], 64)
	}
	info.LoadAverage = parseLoadAvg(readTrim(fsys.LoadAvg))
	return info
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func parseOSRelease(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok && k == "PRETTY_NAME" {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func parseCPUInfo(r io.Reader) proto.CPUInfo {
	var out proto.CPUInfo
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "model name", "Model":
			if out.Model == "" {
				out.Model = strings.TrimSpace(v)
			}
		case "processor":
			out.Cores++
		}
	}
	return out
}

func parseMemInfo(r io.Reader) proto.MemInfo {
	var out proto.MemInfo
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			out.TotalBytes = kb * 1024
		case "MemAvailable:":
			out.AvailableBytes = kb * 1024
		}
	}
	return out
}

func parseLoadAvg(s string) []float64 {
	fields := strings.Fields(s)
	out := make([]float64, 0, 3)
	for i := 0; i < len(fields) && i < 3; i++ {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return nil
		}
		out = append(out, v)
	}
	return out
}
