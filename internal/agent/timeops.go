package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// TimeTools mengatur operasi time.* (agent.time di sapo.yaml): zona waktu
// dan NTP lewat timedatectl (systemd-timedated).
type TimeTools struct {
	Timedatectl string
	Timeout     time.Duration
}

// timezonePattern: bentuk nama zona waktu (misalnya Asia/Jakarta); nilai
// akhirnya harus ada di daftar timedatectl.
var timezonePattern = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+){0,2}$`)

// TimeStatusOp mengembalikan handler time.status.
func TimeStatusOp(t TimeTools) Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		// Semua properti dibaca (filter --property tidak menerima daftar di
		// semua versi timedatectl), yang dibutuhkan dipilih di bawah.
		res, err := runTool(ctx, t.Timeout, t.Timedatectl, []string{"show"})
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("timedatectl show: %s", firstLine(res.Stderr))
		}
		out := proto.TimeStatus{Now: time.Now().UTC()}
		sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
		for sc.Scan() {
			k, v, _ := strings.Cut(sc.Text(), "=")
			switch k {
			case "Timezone":
				if timezonePattern.MatchString(v) {
					out.Timezone = v
				}
			case "NTP":
				out.NTP = v == "yes"
			case "NTPSynchronized":
				out.Synchronized = v == "yes"
			}
		}
		return out, nil
	}
}

func listZones(ctx context.Context, t TimeTools) ([]string, error) {
	res, err := runTool(ctx, t.Timeout, t.Timedatectl, []string{"list-timezones", "--no-pager"})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("timedatectl list-timezones: %s", firstLine(res.Stderr))
	}
	var out []string
	for _, z := range strings.Fields(string(res.Stdout)) {
		if timezonePattern.MatchString(z) {
			out = append(out, z)
		}
	}
	return out, nil
}

// TimeZonesOp mengembalikan handler time.zones.
func TimeZonesOp(t TimeTools) Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		zones, err := listZones(ctx, t)
		if err != nil {
			return nil, err
		}
		return map[string][]string{"zones": zones}, nil
	}
}

// TimeSetOp mengembalikan handler time.set: zona waktu (harus ada di daftar
// timedatectl) dan NTP menyala atau mati.
func TimeSetOp(t TimeTools) Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.TimeSetParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		zones, err := listZones(ctx, t)
		if err != nil {
			return nil, err
		}
		if !timezonePattern.MatchString(p.Timezone) || !slices.Contains(zones, p.Timezone) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "zona waktu tidak dikenal"}
		}
		ntp := "false"
		if p.NTP {
			ntp = "true"
		}
		// NTP dulu: bila tidak didukung, zona waktu belum berubah.
		for _, args := range [][]string{{"set-ntp", ntp}, {"set-timezone", p.Timezone}} {
			res, err := runTool(ctx, t.Timeout, t.Timedatectl, []string{args[0]}, args[1])
			if err != nil {
				return nil, err
			}
			if res.ExitCode != 0 {
				return nil, fmt.Errorf("timedatectl %s: %s", args[0], firstLine(res.Stderr))
			}
		}
		return map[string]bool{"ok": true}, nil
	}
}
