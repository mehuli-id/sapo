package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Backup ke NAS lain, sisi PENERIMA (ADR 0019). Admin membuat "partner" (NAS
// pengirim) dan mendapat kode pemasangan sekali tampil; NAS pengirim memakai
// kode itu untuk menulis salinan ke <pool>/sapo-backup-remote/<partner>/.
//
// Partner hanya bisa: membaca daftar salinannya dan mengirim salinan baru.
// Partner TIDAK bisa menghapus salinan atau mengatur jumlah simpan: NAS
// pengirim yang dibobol tidak bisa menghapus backup-nya sendiri di sini.

const partnerAuthScheme = "Sapo-Backup "

var partnerNameRe = regexp.MustCompile(`^[\p{L}\p{N} ._-]{1,40}$`)

type backupPartner struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Pool string `json:"pool"`
	// Keep: salinan yang diterima kurang dari sekian hari lalu tidak pernah
	// dibuang.
	Keep int `json:"keep"`
	// QuotaGB: batas ruang semua salinan partner (0 = tanpa batas).
	QuotaGB int64 `json:"quota_gb,omitempty"`
	// Revoked: kode dicabut; salinan tetap ada sampai admin menghapusnya.
	Revoked   bool      `json:"revoked,omitempty"`
	TokenHash string    `json:"token_hash"`
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
}

type partnerStore struct {
	path string
	mu   sync.Mutex
	list []backupPartner
	// receiving: satu penerimaan per partner pada satu waktu.
	receiving sync.Map
}

func openPartnerStore(dir string) (*partnerStore, error) {
	st := &partnerStore{path: filepath.Join(dir, "backup-partners.json")}
	data, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &st.list); err != nil {
		return nil, fmt.Errorf("file partner backup rusak (%s): %w", st.path, err)
	}
	return st, nil
}

func (st *partnerStore) all() []backupPartner {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Clone(st.list)
}

func (st *partnerStore) get(id string) (backupPartner, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, p := range st.list {
		if p.ID == id {
			return p, true
		}
	}
	return backupPartner{}, false
}

func (st *partnerStore) mutate(fn func([]backupPartner) []backupPartner) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	next := fn(slices.Clone(st.list))
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(st.path, data); err != nil {
		return err
	}
	st.list = next
	return nil
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// certFingerprint: SHA-256 sertifikat (DER), heksadesimal huruf kecil.
func certFingerprint(der []byte) string {
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:])
}

// pairingCode: SAPO1-<partner>-<token>-<32 heks pertama sidik jari
// sertifikat penerima>. Sidik jari membuat NAS pengirim bisa memastikan ia
// bicara dengan NAS ini sejak sambungan pertama.
func pairingCode(id, token, fp string) string {
	return "SAPO1-" + id + "-" + token + "-" + fp[:32]
}

// authPartner memeriksa header Authorization dari NAS pengirim. Kegagalan
// dibatasi per alamat IP (pembatas login).
func (s *Server) authPartner(w http.ResponseWriter, r *http.Request) (backupPartner, bool) {
	key := "rbackup:" + clientNet(r)
	if rem := s.limiter.Locked(key); rem > 0 {
		s.writeLocked(w, rem)
		return backupPartner{}, false
	}
	fail := func() (backupPartner, bool) {
		s.limiter.Begin(key)
		writeError(w, http.StatusUnauthorized, "unauthorized", "kode partner tidak valid")
		return backupPartner{}, false
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), partnerAuthScheme)
	if !ok {
		return fail()
	}
	id, tok, ok := strings.Cut(raw, ":")
	if !ok {
		return fail()
	}
	p, found := s.partners.get(id)
	want := p.TokenHash
	if !found {
		want = hashToken("sapo-tidak-ada") // waktu perbandingan sama
	}
	if subtle.ConstantTimeCompare([]byte(hashToken(tok)), []byte(want)) != 1 || !found || p.Revoked {
		return fail()
	}
	now := s.now().UTC()
	if now.Sub(p.LastSeen) > time.Minute {
		_ = s.partners.mutate(func(l []backupPartner) []backupPartner {
			for i := range l {
				if l[i].ID == p.ID {
					l[i].LastSeen = now
				}
			}
			return l
		})
	}
	return p, true
}

// isPartnerAPI: permintaan dari NAS pengirim. Tanpa cookie (hanya token per
// permintaan), jadi tidak butuh header CSRF.
func isPartnerAPI(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/v1/rbackup/")
}

func (s *Server) handlePartnerHello(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authPartner(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"partner": p.Name, "host": s.notifyHost(r.Context())})
}

func (s *Server) handlePartnerCopies(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authPartner(w, r)
	if !ok {
		return
	}
	var out proto.RBackupList
	if err := s.agent.Call(r.Context(), proto.OpRBackupList, proto.RBackupTarget{Pool: p.Pool, Partner: p.ID, Job: r.PathValue("job")}, &out); err != nil {
		s.agentError(w, proto.OpRBackupList, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePartnerReceive menerima satu salinan (aliran zfs send dari NAS
// pengirim) dan meneruskannya ke zfs receive di sapod.
func (s *Server) handlePartnerReceive(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authPartner(w, r)
	if !ok {
		return
	}
	if _, busy := s.partners.receiving.LoadOrStore(p.ID, true); busy {
		writeError(w, http.StatusServiceUnavailable, "busy", "penerimaan lain dari NAS ini sedang berjalan")
		return
	}
	defer s.partners.receiving.Delete(p.ID)
	q := r.URL.Query()
	params := proto.RBackupRecvParams{RBackupTarget: proto.RBackupTarget{Pool: p.Pool, Partner: p.ID, Job: r.PathValue("job")}, Base: q.Get("base"), Snap: q.Get("snap"),
		QuotaBytes: uint64(max(p.QuotaGB, 0)) << 30}
	var sess proto.RBackupSession
	f, err := s.agent.CallFile(r.Context(), proto.OpRBackupRecvOpen, params, &sess)
	if err != nil {
		s.agentError(w, proto.OpRBackupRecvOpen, err)
		return
	}
	k := &keepAlive{rc: http.NewResponseController(w)}
	k.touch()
	n, copyErr := io.Copy(f, deadlineReader{r: r.Body, k: k})
	closeErr := f.Close()
	// Diselesaikan walau NAS pengirim memutus sambungan.
	ctx := context.WithoutCancel(r.Context())
	err = s.agent.Call(ctx, proto.OpRBackupRecvFinish, proto.RBackupRecvFinish{Session: sess.Session, OK: copyErr == nil && closeErr == nil, KeepDays: p.Keep}, nil)
	detail := map[string]string{"partner": p.ID, "job": auditText(params.Job), "snap": auditText(params.Snap), "bytes": strconv.FormatInt(n, 10)}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, "partner:"+p.Name, "rbackup.receive", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpRBackupRecvFinish, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bytes": n})
}

// --- Admin penerima -------------------------------------------------------------

type partnerView struct {
	ID       string                 `json:"id"`
	Name     string                 `json:"name"`
	Pool     string                 `json:"pool"`
	Keep     int                    `json:"keep"`
	QuotaGB  int64                  `json:"quota_gb,omitempty"`
	Revoked  bool                   `json:"revoked,omitempty"`
	Created  time.Time              `json:"created"`
	LastSeen time.Time              `json:"last_seen,omitempty"`
	Jobs     []proto.RBackupJobInfo `json:"jobs"`
}

func (s *Server) handlePartners(w http.ResponseWriter, r *http.Request) {
	out := []partnerView{}
	for _, p := range s.partners.all() {
		v := partnerView{ID: p.ID, Name: p.Name, Pool: p.Pool, Keep: p.Keep, QuotaGB: p.QuotaGB, Revoked: p.Revoked, Created: p.Created, LastSeen: p.LastSeen, Jobs: []proto.RBackupJobInfo{}}
		var l proto.RBackupList
		if err := s.agent.Call(r.Context(), proto.OpRBackupList, proto.RBackupTarget{Pool: p.Pool, Partner: p.ID}, &l); err == nil && l.Jobs != nil {
			v.Jobs = l.Jobs
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePartnerCreate membuat partner baru dan mengembalikan kode
// pemasangan (sekali tampil; yang disimpan hanya hash token).
func (s *Server) handlePartnerCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		Pool          string `json:"pool"`
		Keep          int    `json:"keep"`
		QuotaGB       int64  `json:"quota_gb"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.QuotaGB < 0 || req.QuotaGB > 1<<20 {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "kuota tidak valid", map[string]any{"field": "quota_gb"})
		return
	}
	switch {
	case !utf8.ValidString(req.Name) || !partnerNameRe.MatchString(req.Name):
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "nama partner tidak valid", map[string]any{"field": "name"})
		return
	case !shareNameRe.MatchString(req.Pool):
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "pool tidak valid", map[string]any{"field": "pool"})
		return
	case req.Keep < 1 || req.Keep > 366:
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "jumlah simpan 1–366", map[string]any{"field": "keep"})
		return
	case s.https == nil || s.https.Certificate == nil:
		writeError(w, http.StatusConflict, "https_required", "menerima backup butuh HTTPS")
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	idRaw, tokRaw := make([]byte, 6), make([]byte, 24)
	_, _ = rand.Read(idRaw)
	_, _ = rand.Read(tokRaw)
	id, tok := hex.EncodeToString(idRaw), hex.EncodeToString(tokRaw)
	p := backupPartner{ID: id, Name: req.Name, Pool: req.Pool, Keep: req.Keep, QuotaGB: req.QuotaGB, TokenHash: hashToken(tok), Created: s.now().UTC()}
	err := s.partners.mutate(func(l []backupPartner) []backupPartner { return append(l, p) })
	s.record(r, admin.Username, "rbackup.partner_create", err == nil, map[string]string{"partner": id, "name": auditText(req.Name), "pool": req.Pool, "keep": strconv.Itoa(req.Keep), "quota_gb": strconv.FormatInt(req.QuotaGB, 10)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan partner")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "code": pairingCode(id, tok, certFingerprint(s.https.Certificate()))})
}

// handlePartnerDelete mencabut partner; dengan delete_data juga semua
// salinannya (ketik ulang nama partner).
func (s *Server) handlePartnerDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeleteData    bool   `json:"delete_data"`
		ConfirmName   string `json:"confirm_name"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	p, found := s.partners.get(r.PathValue("id"))
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "partner tidak ditemukan")
		return
	}
	if req.DeleteData && req.ConfirmName != p.Name {
		writeErrorDetail(w, http.StatusBadRequest, "confirm_mismatch", "nama yang diketik ulang tidak sama", map[string]any{"field": "confirm_name"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	remove := func() {
		_ = s.partners.mutate(func(l []backupPartner) []backupPartner {
			return slices.DeleteFunc(l, func(x backupPartner) bool { return x.ID == p.ID })
		})
	}
	if !req.DeleteData {
		// Kode dicabut; partner tetap tampil bersama salinannya (bisa
		// dipulihkan, dan dihapus beserta salinan nanti).
		_ = s.partners.mutate(func(l []backupPartner) []backupPartner {
			for i := range l {
				if l[i].ID == p.ID {
					l[i].Revoked, l[i].TokenHash = true, ""
				}
			}
			return l
		})
		s.record(r, admin.Username, "rbackup.partner_delete", true, map[string]string{"partner": p.ID, "delete_data": "false"})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	s.destructive(w, r, admin, "rbackup.partner_delete", map[string]string{"partner": p.ID, "delete_data": "true"},
		proto.OpRBackupDestroy, proto.RBackupTarget{Pool: p.Pool, Partner: p.ID}, remove)
}

// handlePartnerRestore memulihkan salinan dari NAS lain menjadi shared
// folder baru di NAS ini (misalnya saat NAS asal rusak). Izin folder kosong;
// admin memberi izin setelahnya.
func (s *Server) handlePartnerRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Copy          string `json:"copy"`
		Name          string `json:"name"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	p, found := s.partners.get(r.PathValue("id"))
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "partner tidak ditemukan")
		return
	}
	req.Name = strings.TrimSpace(strings.ToLower(req.Name))
	if !shareNameRe.MatchString(req.Name) || slices.Contains(reservedShareNames, req.Name) {
		writeErrorDetail(w, http.StatusBadRequest, "share_invalid", "nama shared folder tidak sesuai aturan", map[string]any{"field": "name"})
		return
	}
	ext, err := s.externalNames(r)
	if err != nil {
		s.agentError(w, proto.OpExternalList, err)
		return
	}
	if _, taken := s.shares.get(req.Name); taken || ext[req.Name] {
		writeErrorDetail(w, http.StatusConflict, "share_taken", "nama shared folder sudah dipakai", map[string]any{"field": "name"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	job := r.PathValue("job")
	err = s.agent.Call(r.Context(), proto.OpRBackupRestore, proto.RBackupRestoreParams{RBackupTarget: proto.RBackupTarget{Pool: p.Pool, Partner: p.ID, Job: job}, Copy: req.Copy, Name: req.Name}, nil)
	detail := map[string]string{"partner": p.ID, "job": auditText(job), "copy": auditText(req.Copy), "share": req.Name}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, admin.Username, "rbackup.restore", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpRBackupRestore, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
