package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mehuli-id/sapo/internal/config"
	"github.com/mehuli-id/sapo/internal/mail"
	"github.com/mehuli-id/sapo/internal/proto"
)

// Notifikasi email (ADR 0018). sapo-api memeriksa kondisi sistem lewat
// operasi baca sapod (pool, SMART, backup), dan mengirim satu email berisi
// semua masalah baru; email "pulih" dikirim saat masalah hilang. Sapo-api
// sendiri yang mengirim email (tidak butuh root); password SMTP disimpan di
// <data_dir>/notify.json (0600, milik akun sapo) dan tidak pernah dikirim
// kembali ke panel.

// NotifySettings adalah pengaturan notifikasi dari Control Panel.
type NotifySettings struct {
	Enabled  bool     `json:"enabled"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Security string   `json:"security"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	// CapacityPct: ambang pool penuh (0 = bawaan konfigurasi).
	CapacityPct int `json:"capacity_pct,omitempty"`
	// EventsOff: jenis kejadian yang tidak dikirim.
	EventsOff []string `json:"events_off,omitempty"`
}

// notifyAlert adalah satu masalah yang sedang berlangsung.
type notifyAlert struct {
	Event  string    `json:"event"`
	Name   string    `json:"name"`
	Detail string    `json:"detail"`
	Since  time.Time `json:"since"`
	// Notified: sudah dikabarkan lewat email; Resolved: sudah hilang tetapi
	// email "pulih" belum terkirim.
	Notified bool `json:"notified"`
	Resolved bool `json:"resolved,omitempty"`
}

type notifyFile struct {
	Settings    NotifySettings          `json:"settings"`
	Alerts      map[string]*notifyAlert `json:"alerts,omitempty"`
	LastSent    time.Time               `json:"last_sent,omitempty"`
	LastError   string                  `json:"last_error,omitempty"`
	LastErrorAt time.Time               `json:"last_error_at,omitempty"`
}

type notifier struct {
	path string
	mu   sync.Mutex
	cur  notifyFile
	// kick: periksa sekarang (setelah pengaturan diubah).
	kick chan struct{}
	// sending: satu pemeriksaan atau email uji pada satu waktu.
	sending   sync.Mutex
	lastSMART time.Time
	// send diganti di test.
	send func(ctx context.Context, s mail.Server, m mail.Message) error
}

func openNotifier(dir string) (*notifier, error) {
	n := &notifier{path: filepath.Join(dir, "notify.json"), kick: make(chan struct{}, 1), send: mail.Send}
	data, err := os.ReadFile(n.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &n.cur); err != nil {
			return nil, fmt.Errorf("file notifikasi rusak (%s): %w", n.path, err)
		}
	}
	if n.cur.Alerts == nil {
		n.cur.Alerts = map[string]*notifyAlert{}
	}
	for k, a := range n.cur.Alerts {
		if a == nil {
			delete(n.cur.Alerts, k)
		}
	}
	return n, nil
}

func (n *notifier) get() notifyFile {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := n.cur
	c.Alerts = make(map[string]*notifyAlert, len(n.cur.Alerts))
	for k, a := range n.cur.Alerts {
		cp := *a
		c.Alerts[k] = &cp
	}
	return c
}

func (n *notifier) update(fn func(*notifyFile)) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	v := n.cur
	v.Alerts = make(map[string]*notifyAlert, len(n.cur.Alerts))
	for k, a := range n.cur.Alerts {
		cp := *a
		v.Alerts[k] = &cp
	}
	fn(&v)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(n.path, data); err != nil {
		return err
	}
	n.cur = v
	return nil
}

func (n *notifier) wake() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

func (st NotifySettings) server() mail.Server {
	return mail.Server{Host: st.Host, Port: st.Port, Security: st.Security, Username: st.Username, Password: st.Password}
}

// messages: teks email dalam bahasa sistem (bahasa setup awal).
func (s *Server) notifyMessages() (config.NotifyMessages, bool) {
	msgs := s.cfg.API.Notify.Messages
	for _, l := range []string{s.store.Language(), s.cfg.API.DefaultLanguage, "id", "en"} {
		if m, ok := msgs[l]; ok && l != "" {
			return m, true
		}
	}
	return config.NotifyMessages{}, false
}

// notifyHost: nama host saat ini dari sapod (sapo-api berjalan dengan
// ProtectHostname, jadi os.Hostname hanya nama saat layanan mulai).
func (s *Server) notifyHost(ctx context.Context) string {
	var info proto.SystemInfo
	if err := s.agent.Call(ctx, proto.OpSystemInfo, nil, &info); err == nil && info.Hostname != "" {
		return info.Hostname
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "sapo"
}

func fill(tmpl string, kv ...string) string { return strings.NewReplacer(kv...).Replace(tmpl) }

// NotifyLoop memeriksa kondisi setiap check_minutes, dan segera setelah
// pengaturan diubah.
func (s *Server) NotifyLoop(ctx context.Context) {
	every := time.Duration(s.cfg.API.Notify.CheckMinutes) * time.Minute
	for {
		s.notifyCheck(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		case <-s.notify.kick:
		}
	}
}

// collect membaca kondisi saat ini. ok[event]: sumber kejadian itu berhasil
// dibaca (hanya kejadian dari sumber yang terbaca yang boleh dianggap pulih).
// cleared: disk yang terbaca sehat (SMART lulus); masalah disk hanya pulih
// untuk disk itu, bukan untuk disk yang tidur, tidak terbaca, atau hilang.
func (s *Server) notifyCollect(ctx context.Context, st NotifySettings, active map[string]*notifyAlert) (cur map[string]*notifyAlert, ok, cleared map[string]bool) {
	cur, ok, cleared = map[string]*notifyAlert{}, map[string]bool{}, map[string]bool{}
	add := func(event, key, name, detail string) {
		cur[event+":"+key] = &notifyAlert{Event: event, Name: name, Detail: detail}
	}

	pct := st.CapacityPct
	if pct == 0 {
		pct = s.cfg.API.Notify.DefaultCapacityPct
	}
	var pools proto.PoolsResult
	if err := s.agent.Call(ctx, proto.OpStoragePools, nil, &pools); err == nil {
		ok["pool_health"], ok["pool_full"] = true, true
		for _, p := range pools.Pools {
			if p.Health != "" && p.Health != "ONLINE" {
				add("pool_health", p.Name, p.Name, p.Health)
			}
			// Histeresis: masalah yang sudah aktif baru pulih 2 poin di bawah
			// ambang (kapasitas yang naik-turun di sekitar ambang tidak
			// membanjiri email).
			if _, was := active["pool_full:"+p.Name]; p.CapacityPct >= pct || (was && p.CapacityPct >= pct-2) {
				add("pool_full", p.Name, p.Name, strconv.Itoa(p.CapacityPct))
			}
		}
	}
	// SMART lebih jarang (disk yang tidur tidak dibangunkan oleh sapod).
	if time.Since(s.notify.lastSMART) >= time.Duration(s.cfg.API.Notify.SMARTMinutes)*time.Minute {
		var disks []proto.Disk
		if err := s.agent.Call(ctx, proto.OpStorageDisks, nil, &disks); err == nil {
			for _, d := range disks {
				key := d.Serial
				if key == "" {
					key = d.Name
				}
				var sm proto.SMART
				if err := s.agent.Call(ctx, proto.OpStorageSMART, proto.SMARTParams{Device: d.Name}, &sm); err != nil {
					continue
				}
				if sm.Status == proto.SMARTPassed {
					cleared["disk_smart:"+key] = true
				}
				if sm.Status == proto.SMARTFailed || sm.Status == proto.SMARTWarning {
					name := strings.TrimSpace(d.Model + " " + d.Serial)
					if name == "" {
						name = d.Name
					}
					detail := sm.Status
					if sm.Message != "" {
						detail += ", " + sm.Message
					}
					add("disk_smart", key, name+" ("+d.Name+")", detail)
				}
			}
			ok["disk_smart"] = true
			s.notify.lastSMART = time.Now()
		}
	}
	var bk proto.BackupsListResult
	// sapod lama (operasi tidak dikenal) = tidak ada data, bukan "pulih".
	if err := s.agent.Call(ctx, proto.OpBackupsList, nil, &bk); err == nil {
		ok["backup_failed"] = true
		for _, j := range bk.Jobs {
			if !j.LastOK && j.LastError != "" {
				add("backup_failed", j.ID, j.Share+" → "+j.DestPool, j.LastError)
			}
		}
	}
	return cur, ok, cleared
}

// notifyCheck: satu putaran pemeriksaan dan (bila perlu) satu email.
func (s *Server) notifyCheck(ctx context.Context) {
	if !s.notify.sending.TryLock() {
		return
	}
	defer s.notify.sending.Unlock()
	st := s.notify.get().Settings
	if !st.Enabled {
		return
	}
	cur, ok, cleared := s.notifyCollect(ctx, st, s.notify.get().Alerts)
	now := s.now().UTC()
	var fresh, resolved []*notifyAlert
	// Bila catatan tidak bisa disimpan, tidak ada email: tanpa catatan, email
	// yang sama akan terkirim setiap putaran.
	if err := s.notify.update(func(f *notifyFile) {
		for k, a := range cur {
			if slices.Contains(st.EventsOff, a.Event) {
				continue
			}
			if old, exists := f.Alerts[k]; exists {
				old.Detail, old.Resolved = a.Detail, false
				continue
			}
			a.Since = now
			f.Alerts[k] = a
		}
		for k, a := range f.Alerts {
			if slices.Contains(st.EventsOff, a.Event) {
				delete(f.Alerts, k)
				continue
			}
			if _, still := cur[k]; !still && ok[a.Event] && (a.Event != "disk_smart" || cleared[k]) {
				if !a.Notified {
					delete(f.Alerts, k)
					continue
				}
				a.Resolved = true
			}
		}
		for _, a := range f.Alerts {
			switch {
			case a.Resolved:
				resolved = append(resolved, a)
			case !a.Notified:
				fresh = append(fresh, a)
			}
		}
	}); err != nil {
		if s.log != nil {
			s.log.Error("catatan notifikasi tidak bisa disimpan; email ditunda", "err", err)
		}
		return
	}
	if len(fresh) == 0 && len(resolved) == 0 {
		return
	}
	msgs, found := s.notifyMessages()
	if !found {
		s.notifyResult(errors.New("teks email tidak ada di konfigurasi (api.notify.messages)"), nil, nil)
		return
	}
	host := s.notifyHost(ctx)
	order := func(l []*notifyAlert) {
		sort.Slice(l, func(i, j int) bool { return l[i].Event+l[i].Name < l[j].Event+l[j].Name })
	}
	order(fresh)
	order(resolved)
	line := func(a *notifyAlert) string {
		return "- " + fill(msgs.Events[a.Event], "{name}", a.Name, "{detail}", a.Detail, "{host}", host)
	}
	var b strings.Builder
	subject := fill(msgs.SubjectResolved, "{host}", host)
	if len(fresh) > 0 {
		subject = fill(msgs.SubjectAlert, "{host}", host, "{n}", strconv.Itoa(len(fresh)))
		b.WriteString(fill(msgs.Intro, "{host}", host) + "\n\n")
		for _, a := range fresh {
			b.WriteString(line(a) + "\n")
		}
		b.WriteString("\n")
	}
	if len(resolved) > 0 {
		b.WriteString(msgs.ResolvedHeader + "\n\n")
		for _, a := range resolved {
			b.WriteString(line(a) + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(msgs.Footer + "\n")
	err := s.notify.send(ctx, st.server(), mail.Message{From: st.From, To: st.To, Subject: subject, Body: b.String()})
	s.notifyResult(err, fresh, resolved)
}

// notifyResult mencatat hasil kirim: yang terkirim ditandai, yang gagal
// dicoba lagi pada pemeriksaan berikutnya.
func (s *Server) notifyResult(err error, fresh, resolved []*notifyAlert) {
	now := s.now().UTC()
	_ = s.notify.update(func(f *notifyFile) {
		if err != nil {
			f.LastError, f.LastErrorAt = clipText(err.Error(), 300), now
			return
		}
		f.LastSent, f.LastError, f.LastErrorAt = now, "", time.Time{}
		for k, a := range f.Alerts {
			for _, x := range fresh {
				if x.Event == a.Event && x.Name == a.Name && !a.Resolved {
					a.Notified = true
				}
			}
			for _, x := range resolved {
				if x.Event == a.Event && x.Name == a.Name && a.Resolved {
					delete(f.Alerts, k)
				}
			}
		}
	})
	if err != nil && s.log != nil {
		s.log.Warn("email notifikasi gagal dikirim", "err", err)
	}
}

// --- Handler -----------------------------------------------------------------

type notifyView struct {
	Settings           NotifySettings `json:"settings"`
	HasPassword        bool           `json:"has_password"`
	Events             []string       `json:"events"`
	DefaultCapacityPct int            `json:"default_capacity_pct"`
	MaxRecipients      int            `json:"max_recipients"`
	LastSent           time.Time      `json:"last_sent,omitempty"`
	LastError          string         `json:"last_error,omitempty"`
	LastErrorAt        time.Time      `json:"last_error_at,omitempty"`
	Alerts             []notifyAlert  `json:"alerts"`
}

func (s *Server) handleNotify(w http.ResponseWriter, _ *http.Request) {
	f := s.notify.get()
	v := notifyView{Settings: f.Settings, HasPassword: f.Settings.Password != "", Events: config.NotifyEvents,
		DefaultCapacityPct: s.cfg.API.Notify.DefaultCapacityPct, MaxRecipients: s.cfg.API.Notify.MaxRecipients,
		LastSent: f.LastSent, LastError: f.LastError, LastErrorAt: f.LastErrorAt, Alerts: []notifyAlert{}}
	v.Settings.Password = ""
	if v.Settings.To == nil {
		v.Settings.To = []string{}
	}
	for _, a := range f.Alerts {
		if !a.Resolved {
			v.Alerts = append(v.Alerts, *a)
		}
	}
	sort.Slice(v.Alerts, func(i, j int) bool { return v.Alerts[i].Since.Before(v.Alerts[j].Since) })
	writeJSON(w, http.StatusOK, v)
}

// handleNotifySet menyimpan pengaturan. Butuh password admin. Password SMTP
// kosong = tetap memakai yang tersimpan, tetapi hanya bila server dan
// username tidak berubah (password tersimpan tidak pernah dikirim ke server
// lain).
func (s *Server) handleNotifySet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NotifySettings
		ClearPassword bool   `json:"clear_password"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	in := req.NotifySettings
	in.Host = strings.TrimSpace(in.Host)
	in.Username = strings.TrimSpace(in.Username)
	from, err := mail.ParseAddress(in.From)
	if err != nil {
		writeErrorDetail(w, http.StatusBadRequest, "email_invalid", "alamat pengirim tidak valid", map[string]any{"field": "from"})
		return
	}
	in.From = from
	var to []string
	for _, a := range in.To {
		if strings.TrimSpace(a) == "" {
			continue
		}
		p, err := mail.ParseAddress(a)
		if err != nil {
			writeErrorDetail(w, http.StatusBadRequest, "email_invalid", "alamat penerima tidak valid", map[string]any{"field": "to"})
			return
		}
		if !slices.Contains(to, p) {
			to = append(to, p)
		}
	}
	if len(to) == 0 || len(to) > s.cfg.API.Notify.MaxRecipients {
		writeErrorDetail(w, http.StatusBadRequest, "email_recipients", "jumlah penerima tidak sesuai", map[string]any{"field": "to", "max": s.cfg.API.Notify.MaxRecipients})
		return
	}
	in.To = to
	if in.CapacityPct < 0 || in.CapacityPct > 99 {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "ambang pool penuh 1–99", map[string]any{"field": "capacity_pct"})
		return
	}
	var off []string
	for _, e := range in.EventsOff {
		if slices.Contains(config.NotifyEvents, e) && !slices.Contains(off, e) {
			off = append(off, e)
		}
	}
	in.EventsOff = off
	old := s.notify.get().Settings
	if in.Password == "" && !req.ClearPassword && in.Username != "" {
		if old.Password == "" || old.Host != in.Host || old.Username != in.Username || old.Port != in.Port || old.Security != in.Security {
			writeErrorDetail(w, http.StatusBadRequest, "smtp_password_required", "password SMTP wajib diisi untuk server, port, keamanan, atau username baru", map[string]any{"field": "password"})
			return
		}
		in.Password = old.Password
	}
	if in.Username == "" {
		in.Password = ""
	}
	if err := mail.ValidServer(in.server()); err != nil {
		writeErrorDetail(w, http.StatusBadRequest, "smtp_invalid", err.Error(), map[string]any{"field": "host"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	err = s.notify.update(func(f *notifyFile) {
		f.Settings = in
		if !in.Enabled {
			// Mati: masalah yang tercatat dilupakan (dinyalakan lagi = mulai baru).
			f.Alerts = map[string]*notifyAlert{}
		}
	})
	s.record(r, admin.Username, "notify.set", err == nil, map[string]string{"enabled": strconv.FormatBool(in.Enabled),
		"host": in.Host, "port": strconv.Itoa(in.Port), "security": in.Security, "to": strings.Join(in.To, ","), "events_off": strings.Join(in.EventsOff, ",")})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan pengaturan notifikasi")
		return
	}
	s.notify.wake()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleNotifyTest mengirim email uji dengan pengaturan tersimpan.
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	st := s.notify.get().Settings
	if st.Host == "" || len(st.To) == 0 {
		writeError(w, http.StatusBadRequest, "notify_not_configured", "notifikasi email belum diatur")
		return
	}
	msgs, found := s.notifyMessages()
	if !found {
		writeError(w, http.StatusInternalServerError, "internal", "teks email tidak ada di konfigurasi")
		return
	}
	if !s.notify.sending.TryLock() {
		writeError(w, http.StatusServiceUnavailable, "busy", "email lain sedang dikirim")
		return
	}
	defer s.notify.sending.Unlock()
	host := s.notifyHost(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(55 * time.Second))
	err := s.notify.send(ctx, st.server(), mail.Message{From: st.From, To: st.To,
		Subject: fill(msgs.SubjectTest, "{host}", host), Body: fill(msgs.Test, "{host}", host) + "\n\n" + msgs.Footer + "\n"})
	sess, _, _ := s.currentSession(r)
	detail := map[string]string{"host": st.Host, "to": strings.Join(st.To, ",")}
	if err != nil {
		detail["error"] = clipText(err.Error(), 200)
	}
	s.record(r, sess.Username, "notify.test", err == nil, detail)
	if err != nil {
		writeError(w, http.StatusBadGateway, "notify_failed", clipText(err.Error(), 300))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
