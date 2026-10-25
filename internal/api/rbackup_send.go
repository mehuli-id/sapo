package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Backup ke NAS lain, sisi PENGIRIM (ADR 0019). Admin menambahkan NAS tujuan
// dengan alamat dan kode pemasangan dari NAS itu; sidik jari sertifikat
// tujuan dikunci saat itu. Tugas menyalin satu shared folder ke tujuan
// setiap hari: sapod menjalankan zfs send, sapo-api mengalirkannya lewat
// HTTPS ke NAS tujuan.

var pairingCodeRe = regexp.MustCompile(`^SAPO1-([a-z0-9]{8,32})-([0-9a-f]{40,64})-([0-9a-f]{32})$`)

type backupTarget struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Address     string    `json:"address"` // host:port HTTPS NAS tujuan
	Partner     string    `json:"partner"`
	Token       string    `json:"token"`
	Fingerprint string    `json:"fingerprint"`
	RemoteHost  string    `json:"remote_host,omitempty"`
	Created     time.Time `json:"created"`
}

type remoteJob struct {
	ID          string    `json:"id"`
	Share       string    `json:"share"`
	Pool        string    `json:"pool"`
	Target      string    `json:"target"`
	Hour        int       `json:"hour"`
	Enabled     bool      `json:"enabled"`
	LastRun     time.Time `json:"last_run,omitempty"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastOK      bool      `json:"last_ok"`
	LastError   string    `json:"last_error,omitempty"`
	LastBytes   int64     `json:"last_bytes,omitempty"`
	LastCopies  int       `json:"last_copies,omitempty"`
}

type remoteFile struct {
	Targets []backupTarget `json:"targets"`
	Jobs    []remoteJob    `json:"jobs"`
}

type remoteStore struct {
	path string
	mu   sync.Mutex
	cur  remoteFile
	// running: tugas yang sedang berjalan; one: satu pengiriman pada satu
	// waktu.
	running sync.Map
	one     chan struct{}
}

func openRemoteStore(dir string) (*remoteStore, error) {
	st := &remoteStore{path: filepath.Join(dir, "backup-remote.json"), one: make(chan struct{}, 1)}
	data, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &st.cur); err != nil {
			return nil, fmt.Errorf("file backup jarak jauh rusak (%s): %w", st.path, err)
		}
	}
	return st, nil
}

func (st *remoteStore) get() remoteFile {
	st.mu.Lock()
	defer st.mu.Unlock()
	return remoteFile{Targets: slices.Clone(st.cur.Targets), Jobs: slices.Clone(st.cur.Jobs)}
}

func (st *remoteStore) mutate(fn func(*remoteFile) error) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	v := remoteFile{Targets: slices.Clone(st.cur.Targets), Jobs: slices.Clone(st.cur.Jobs)}
	if err := fn(&v); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(st.path, data); err != nil {
		return err
	}
	st.cur = v
	return nil
}

func (f remoteFile) target(id string) (backupTarget, bool) {
	for _, t := range f.Targets {
		if t.ID == id {
			return t, true
		}
	}
	return backupTarget{}, false
}

// usingShare: tugas backup jarak jauh yang memakai shared folder name.
func (st *remoteStore) usingShare(name string) string {
	for _, j := range st.get().Jobs {
		if j.Share == name {
			return j.ID
		}
	}
	return ""
}

// normalizeAddress: host[:port] (port bawaan 5001), tanpa skema atau path.
func normalizeAddress(a string) (string, error) {
	a = strings.TrimSpace(a)
	a = strings.TrimPrefix(a, "https://")
	a = strings.TrimSuffix(a, "/")
	host, port, err := net.SplitHostPort(a)
	if err != nil {
		host, port = strings.Trim(a, "[]"), "5001"
	}
	if host == "" || strings.ContainsAny(host, "/@?#% \t") || len(host) > 253 {
		return "", errors.New("alamat tidak valid")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", errors.New("port tidak valid")
	}
	return net.JoinHostPort(host, port), nil
}

// pinnedClient: klien HTTPS yang hanya menerima sertifikat dengan sidik jari
// fp (heks penuh), atau, saat pemasangan (prefix=true), yang sidik jarinya
// berawalan fp. seen menerima sidik jari penuh yang terlihat.
func pinnedClient(fp string, prefix bool, seen *string) *http.Client {
	verify := func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("NAS tujuan tidak mengirim sertifikat")
		}
		got := certFingerprint(cs.PeerCertificates[0].Raw)
		if seen != nil {
			*seen = got
		}
		if (prefix && strings.HasPrefix(got, fp)) || (!prefix && got == fp) {
			return nil
		}
		return errors.New("sertifikat NAS tujuan tidak cocok dengan yang dipasangkan (sertifikatnya berganti? pasangkan ulang)")
	}
	return &http.Client{
		Transport: &http.Transport{
			// Sertifikat Sapo self-signed: kepercayaan lewat sidik jari yang
			// dikunci (VerifyConnection), bukan lewat CA.
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: verify},
			DialContext:         (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
			TLSHandshakeTimeout: 15 * time.Second,
			// Jawaban setelah unggahan selesai: penerima hanya menunggu zfs
			// receive selesai.
			ResponseHeaderTimeout: 10 * time.Minute,
			// Klien baru per tugas: tanpa koneksi menganggur yang tertinggal.
			DisableKeepAlives: true,
			Proxy:             nil,
		},
		// Tidak mengikuti pengalihan ke tempat lain.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// partnerRequest: satu permintaan ke NAS tujuan dengan token partner.
func (s *Server) partnerRequest(ctx context.Context, c *http.Client, t backupTarget, method, path string, q url.Values, body io.Reader, out any) error {
	u := url.URL{Scheme: "https", Host: t.Address, Path: path, RawQuery: q.Encode()}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", partnerAuthScheme+t.Partner+":"+t.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	res, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("NAS tujuan tidak bisa dihubungi: %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error.Code == "" {
			e.Error.Code, e.Error.Message = "remote_error", "NAS tujuan menjawab "+res.Status
		}
		return &proto.Error{Code: e.Error.Code, Message: "NAS tujuan: " + clipText(e.Error.Message, 300)}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (s *Server) remoteCopies(ctx context.Context, c *http.Client, t backupTarget, job string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var l proto.RBackupList
	if err := s.partnerRequest(ctx, c, t, http.MethodGet, "/api/v1/rbackup/jobs/"+job+"/copies", nil, nil, &l); err != nil {
		return nil, err
	}
	var names []string
	for _, c := range l.Copies {
		names = append(names, c.Name)
	}
	return names, nil
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// runRemoteJob: satu backup ke NAS lain.
func (s *Server) runRemoteJob(ctx context.Context, id string) error {
	select {
	case s.remote.one <- struct{}{}:
	default:
		return &proto.Error{Code: proto.ErrBusy, Message: "backup ke NAS lain sedang berjalan"}
	}
	defer func() { <-s.remote.one }()
	f := s.remote.get()
	idx := slices.IndexFunc(f.Jobs, func(j remoteJob) bool { return j.ID == id })
	if idx < 0 {
		return &proto.Error{Code: proto.ErrNotFound, Message: "tugas tidak ditemukan"}
	}
	j := f.Jobs[idx]
	s.remote.running.Store(id, true)
	defer s.remote.running.Delete(id)
	start := s.now().UTC()
	n, copies, err := s.sendRemote(ctx, f, j)
	_ = s.remote.mutate(func(v *remoteFile) error {
		for i := range v.Jobs {
			if v.Jobs[i].ID != id {
				continue
			}
			x := &v.Jobs[i]
			x.LastAttempt, x.LastOK, x.LastBytes, x.LastError = start, err == nil, n, ""
			if err != nil {
				x.LastError = clipText(err.Error(), 300)
			} else {
				x.LastRun, x.LastCopies = start, copies
			}
		}
		return nil
	})
	if err != nil && s.log != nil {
		s.log.Warn("backup ke NAS lain gagal", "tugas", id, "err", err)
	}
	return err
}

func (s *Server) sendRemote(ctx context.Context, f remoteFile, j remoteJob) (int64, int, error) {
	t, ok := f.target(j.Target)
	if !ok {
		return 0, 0, errors.New("NAS tujuan tugas ini sudah dihapus")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.Agent.Backup.MaxRunHours)*time.Hour)
	defer cancel()
	c := pinnedClient(t.Fingerprint, false, nil)
	have, err := s.remoteCopies(ctx, c, t, j.ID)
	if err != nil {
		return 0, 0, err
	}
	var sess proto.RBackupSession
	file, err := s.agent.CallFile(ctx, proto.OpRBackupSendOpen, proto.RBackupSendParams{Pool: j.Pool, Share: j.Share, Job: j.ID, Remote: have}, &sess)
	if err != nil {
		return 0, 0, err
	}
	body := &countingReader{r: file}
	q := url.Values{"base": {sess.Base}, "snap": {sess.Snap}}
	postErr := s.partnerRequest(ctx, c, t, http.MethodPost, "/api/v1/rbackup/jobs/"+j.ID+"/receive", q, io.NopCloser(body), nil)
	file.Close()
	// Penerima menyimpan snapshot ini? (bisa sudah tersimpan walau jawaban
	// tidak sampai).
	after, listErr := s.remoteCopies(context.WithoutCancel(ctx), c, t, j.ID)
	remoteHas := listErr == nil && slices.Contains(after, sess.Snap)
	finErr := s.agent.Call(context.WithoutCancel(ctx), proto.OpRBackupSendFinish, proto.RBackupSendFinish{Session: sess.Session, OK: postErr == nil, RemoteHas: remoteHas}, nil)
	switch {
	case remoteHas:
		return body.n.Load(), len(after), nil
	case postErr != nil:
		return body.n.Load(), 0, postErr
	case finErr != nil:
		return body.n.Load(), 0, finErr
	}
	return body.n.Load(), 0, errors.New("NAS tujuan tidak menyimpan salinan")
}

// dailyDue: jam hour hari ini (waktu NAS) sudah lewat dan belum ada backup
// berhasil sejak itu; percobaan gagal diulang paling cepat sejam kemudian.
func dailyDue(lastOK, lastAttempt time.Time, hour int, now time.Time, loc *time.Location) bool {
	l := now.In(loc)
	start := time.Date(l.Year(), l.Month(), l.Day(), hour, 0, 0, 0, loc)
	if start.After(l) {
		start = start.AddDate(0, 0, -1)
	}
	if !lastOK.Before(start) {
		return false
	}
	return lastAttempt.IsZero() || lastAttempt.Before(start) || now.Sub(lastAttempt) >= time.Hour
}

// RemoteBackupLoop menjalankan tugas yang jatuh tempo, diperiksa tiap menit.
func (s *Server) RemoteBackupLoop(ctx context.Context) {
	for {
		if f := s.remote.get(); len(f.Jobs) > 0 {
			loc := s.nasLocation(ctx)
			for _, j := range f.Jobs {
				if j.Enabled && dailyDue(j.LastRun, j.LastAttempt, j.Hour, s.now(), loc) {
					_ = s.runRemoteJob(ctx, j.ID)
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

// --- Handler pengirim -----------------------------------------------------------

type targetView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Address     string    `json:"address"`
	Fingerprint string    `json:"fingerprint"`
	RemoteHost  string    `json:"remote_host,omitempty"`
	Created     time.Time `json:"created"`
}

type remoteJobView struct {
	remoteJob
	TargetName string `json:"target_name"`
	Running    bool   `json:"running"`
}

func (s *Server) handleRemoteBackups(w http.ResponseWriter, _ *http.Request) {
	f := s.remote.get()
	out := struct {
		Targets []targetView    `json:"targets"`
		Jobs    []remoteJobView `json:"jobs"`
	}{Targets: []targetView{}, Jobs: []remoteJobView{}}
	for _, t := range f.Targets {
		// Token tidak pernah dikirim ke panel.
		out.Targets = append(out.Targets, targetView{ID: t.ID, Name: t.Name, Address: t.Address, Fingerprint: t.Fingerprint, RemoteHost: t.RemoteHost, Created: t.Created})
	}
	for _, j := range f.Jobs {
		t, _ := f.target(j.Target)
		_, running := s.remote.running.Load(j.ID)
		out.Jobs = append(out.Jobs, remoteJobView{remoteJob: j, TargetName: t.Name, Running: running})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTargetAdd memasangkan NAS tujuan: alamat + kode dari NAS itu.
// Sertifikat tujuan harus cocok dengan sidik jari di kode; sidik jari
// penuhnya lalu dikunci.
func (s *Server) handleTargetAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		Address       string `json:"address"`
		Code          string `json:"code"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !utf8.ValidString(req.Name) || !partnerNameRe.MatchString(req.Name) {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "nama tidak valid", map[string]any{"field": "name"})
		return
	}
	addr, err := normalizeAddress(req.Address)
	if err != nil {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", err.Error(), map[string]any{"field": "address"})
		return
	}
	m := pairingCodeRe.FindStringSubmatch(strings.TrimSpace(req.Code))
	if m == nil {
		writeErrorDetail(w, http.StatusBadRequest, "pairing_code_invalid", "kode pemasangan tidak valid", map[string]any{"field": "code"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	t := backupTarget{Name: req.Name, Address: addr, Partner: m[1], Token: m[2], Created: s.now().UTC()}
	var seen string
	var hello struct {
		Host string `json:"host"`
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err = s.partnerRequest(ctx, pinnedClient(m[3], true, &seen), t, http.MethodGet, "/api/v1/rbackup/hello", nil, nil, &hello)
	detail := map[string]string{"address": addr, "partner": m[1]}
	if err != nil {
		detail["error"] = clipText(err.Error(), 200)
		s.record(r, admin.Username, "rbackup.target_add", false, detail)
		writeError(w, http.StatusBadGateway, "pairing_failed", clipText(err.Error(), 300))
		return
	}
	t.Fingerprint, t.RemoteHost = seen, clipText(hello.Host, 64)
	t.ID = m[1]
	err = s.remote.mutate(func(v *remoteFile) error {
		if _, exists := v.target(t.ID); exists {
			return errShareExists
		}
		v.Targets = append(v.Targets, t)
		return nil
	})
	if errors.Is(err, errShareExists) {
		writeError(w, http.StatusConflict, "target_exists", "NAS tujuan ini sudah dipasangkan")
		return
	}
	detail["fingerprint"] = seen
	s.record(r, admin.Username, "rbackup.target_add", err == nil, detail)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan")
		return
	}
	writeJSON(w, http.StatusOK, targetView{ID: t.ID, Name: t.Name, Address: t.Address, Fingerprint: t.Fingerprint, RemoteHost: t.RemoteHost, Created: t.Created})
}

func (s *Server) handleTargetDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.remote.mutate(func(v *remoteFile) error {
		if slices.ContainsFunc(v.Jobs, func(j remoteJob) bool { return j.Target == id }) {
			return errTargetUsed
		}
		v.Targets = slices.DeleteFunc(v.Targets, func(t backupTarget) bool { return t.ID == id })
		return nil
	})
	if errors.Is(err, errTargetUsed) {
		writeError(w, http.StatusConflict, "target_used", "NAS tujuan masih dipakai tugas backup")
		return
	}
	sess, _, _ := s.currentSession(r)
	s.record(r, sess.Username, "rbackup.target_delete", err == nil, map[string]string{"target": auditText(id)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

var errTargetUsed = errors.New("tujuan dipakai")

// handleRemoteJobSet membuat atau mengubah tugas. Butuh password admin
// (data keluar dari NAS ini).
func (s *Server) handleRemoteJobSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID            string `json:"id"`
		Share         string `json:"share"`
		Target        string `json:"target"`
		Hour          int    `json:"hour"`
		Enabled       bool   `json:"enabled"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sh, ok := s.shares.get(req.Share)
	if !ok {
		writeErrorDetail(w, http.StatusNotFound, "not_found", "shared folder tidak ditemukan", map[string]any{"field": "share"})
		return
	}
	if _, ok := s.remote.get().target(req.Target); !ok {
		writeErrorDetail(w, http.StatusNotFound, "not_found", "NAS tujuan tidak ditemukan", map[string]any{"field": "target"})
		return
	}
	if req.Hour < 0 || req.Hour > 23 {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "jam 0–23", map[string]any{"field": "hour"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	id := req.ID
	err := s.remote.mutate(func(v *remoteFile) error {
		if id != "" {
			for i := range v.Jobs {
				if v.Jobs[i].ID == id {
					if v.Jobs[i].Share != req.Share || v.Jobs[i].Target != req.Target {
						return errTargetUsed
					}
					v.Jobs[i].Hour, v.Jobs[i].Enabled = req.Hour, req.Enabled
					return nil
				}
			}
			return errShareMissing
		}
		id = backupID(sh.Name, "r"+req.Target[:6])
		if slices.ContainsFunc(v.Jobs, func(j remoteJob) bool { return j.ID == id }) {
			return errShareExists
		}
		v.Jobs = append(v.Jobs, remoteJob{ID: id, Share: sh.Name, Pool: sh.Pool, Target: req.Target, Hour: req.Hour, Enabled: req.Enabled})
		return nil
	})
	switch {
	case errors.Is(err, errShareExists):
		writeError(w, http.StatusConflict, "conflict", "folder ini sudah di-backup ke NAS itu")
		return
	case errors.Is(err, errTargetUsed):
		writeError(w, http.StatusConflict, "conflict", "folder dan tujuan tugas yang sudah ada tidak bisa diubah")
		return
	case errors.Is(err, errShareMissing):
		writeError(w, http.StatusNotFound, "not_found", "tugas tidak ditemukan")
		return
	}
	s.record(r, admin.Username, "rbackup.job_set", err == nil, map[string]string{"id": id, "share": sh.Name, "target": req.Target,
		"hour": strconv.Itoa(req.Hour), "enabled": strconv.FormatBool(req.Enabled)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

var errShareMissing = errors.New("tidak ada")

func (s *Server) handleRemoteJobRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !slices.ContainsFunc(s.remote.get().Jobs, func(j remoteJob) bool { return j.ID == id }) {
		writeError(w, http.StatusNotFound, "not_found", "tugas tidak ditemukan")
		return
	}
	if _, running := s.remote.running.Load(id); running || len(s.remote.one) > 0 {
		writeError(w, http.StatusServiceUnavailable, "busy", "backup ke NAS lain sedang berjalan")
		return
	}
	sess, _, _ := s.currentSession(r)
	s.record(r, sess.Username, "rbackup.run", true, map[string]string{"id": auditText(id)})
	go func() { _ = s.runRemoteJob(context.Background(), id) }()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRemoteJobDelete menghapus tugas dan snapshot backup-nya di folder
// asal. Salinan di NAS tujuan TIDAK dihapus dari sini (hanya admin NAS
// tujuan yang bisa).
func (s *Server) handleRemoteJobDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	f := s.remote.get()
	idx := slices.IndexFunc(f.Jobs, func(j remoteJob) bool { return j.ID == id })
	if idx < 0 {
		writeError(w, http.StatusNotFound, "not_found", "tugas tidak ditemukan")
		return
	}
	if _, running := s.remote.running.Load(id); running {
		writeError(w, http.StatusServiceUnavailable, "busy", "tugas sedang berjalan")
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	j := f.Jobs[idx]
	_ = s.agent.Call(r.Context(), proto.OpRBackupSendClean, proto.RBackupSendParams{Pool: j.Pool, Share: j.Share, Job: j.ID}, nil)
	err := s.remote.mutate(func(v *remoteFile) error {
		v.Jobs = slices.DeleteFunc(v.Jobs, func(x remoteJob) bool { return x.ID == id })
		return nil
	})
	s.record(r, admin.Username, "rbackup.job_delete", err == nil, map[string]string{"id": id, "share": j.Share, "target": j.Target})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
