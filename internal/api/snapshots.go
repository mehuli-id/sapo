package api

import (
	"net/http"
	"strconv"

	"github.com/mehuli-id/sapo/internal/proto"
)

// --- Snapshot ZFS terjadwal (ADR 0014) ----------------------------------------
//
// Jadwal snapshot shared folder disimpan bersama folder (shares.json), jadwal
// folder pribadi di settings.json; keduanya dikirim ke sapod lewat
// sinkronisasi (shares.apply, homes.apply), dan sapod yang mengambil serta
// membuang snapshot sesuai jadwal. Hanya admin yang mengatur dan melihatnya
// di sini.

func policyText(p *proto.SnapshotPolicy) string {
	if p == nil {
		return "off"
	}
	return "h" + strconv.Itoa(p.Hourly) + " d" + strconv.Itoa(p.Daily) + " w" + strconv.Itoa(p.Weekly) + " t" + strconv.Itoa(p.Hour)
}

// normalizePolicy: jadwal tanpa satu jenis pun = mati (nil).
func normalizePolicy(p *proto.SnapshotPolicy) (*proto.SnapshotPolicy, bool) {
	if p == nil {
		return nil, true
	}
	if !p.Valid() {
		return nil, false
	}
	if p.Hourly == 0 && p.Daily == 0 && p.Weekly == 0 {
		return nil, true
	}
	c := *p
	return &c, true
}

// prunes: jenis yang akan membuang snapshot otomatis yang ada bila jadwal
// diganti dari old ke next. have: jumlah snapshot otomatis per jenis (nil =
// tidak diketahui, dianggap ada).
func prunes(old, next *proto.SnapshotPolicy, have map[string]int) bool {
	if next == nil {
		return false
	}
	var o proto.SnapshotPolicy
	if old != nil {
		o = *old
	}
	for _, k := range []struct {
		kind      string
		old, next int
	}{{"hourly", o.Hourly, next.Hourly}, {"daily", o.Daily, next.Daily}, {"weekly", o.Weekly, next.Weekly}} {
		if k.next == 0 {
			continue
		}
		if have != nil {
			if have[k.kind] > k.next {
				return true
			}
			continue
		}
		if k.old == 0 || k.next < k.old {
			return true
		}
	}
	return false
}

// confirmPrune: perubahan jadwal yang akan membuang snapshot yang ada adalah
// operasi destruktif: butuh password admin. Mengembalikan pelaku.
func (s *Server) confirmPrune(w http.ResponseWriter, r *http.Request, needed bool, password string) (string, bool) {
	sess, _, _ := s.currentSession(r)
	if !needed {
		return sess.Username, true
	}
	if password == "" {
		writeErrorDetail(w, http.StatusUnauthorized, "snapshot_prune_confirm", "jadwal baru akan membuang snapshot yang ada; masukkan password", map[string]any{"field": "admin_password"})
		return "", false
	}
	admin, ok := s.reauthField(w, r, password, "admin_password")
	return admin.Username, ok
}

func (s *Server) handleSnapshotPresets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg.API.SnapshotPresets)
}

// handleSharePolicy mengatur jadwal snapshot satu shared folder (null =
// mati; snapshot yang sudah ada disimpan).
func (s *Server) handleSharePolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policy        *proto.SnapshotPolicy `json:"policy"`
		AdminPassword string                `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	pol, ok := normalizePolicy(req.Policy)
	if !ok {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "jadwal snapshot tidak valid", map[string]any{"field": "policy"})
		return
	}
	name := r.PathValue("name")
	sh, ok := s.shares.get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "shared folder tidak ditemukan")
		return
	}
	// Jumlah snapshot otomatis yang ada; bila tidak terbaca, dianggap ada.
	var have map[string]int
	var list proto.SnapshotsListResult
	if err := s.agent.Call(r.Context(), proto.OpSnapshotsList, proto.SnapshotTarget{Pool: sh.Pool, Share: name}, &list); err == nil {
		have = map[string]int{}
		for _, sn := range list.Snapshots {
			have[sn.Kind]++
		}
	}
	actor, ok := s.confirmPrune(w, r, prunes(sh.Snapshots, pol, have), req.AdminPassword)
	if !ok {
		return
	}
	if err := s.shares.mutate(func(cur []Share) ([]Share, error) {
		for i := range cur {
			if cur[i].Name == name {
				cur[i].Snapshots = pol
			}
		}
		return cur, nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan shared folder")
		return
	}
	warning := s.syncAfterChange(r)
	s.record(r, actor, "snapshots.policy", warning == "", map[string]string{"share": name, "policy": policyText(pol)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
}

// handleDrivePolicy mengatur jadwal snapshot semua folder pribadi.
func (s *Server) handleDrivePolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policy        *proto.SnapshotPolicy `json:"policy"`
		AdminPassword string                `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	pol, ok := normalizePolicy(req.Policy)
	if !ok {
		writeErrorDetail(w, http.StatusBadRequest, "bad_request", "jadwal snapshot tidak valid", map[string]any{"field": "policy"})
		return
	}
	// Banyak folder pribadi: jumlah snapshot yang ada tidak diperiksa satu per
	// satu, jadi setiap penurunan atau jenis baru dianggap membuang.
	actor, ok := s.confirmPrune(w, r, prunes(s.settings.get().HomesSnapshots, pol, nil), req.AdminPassword)
	if !ok {
		return
	}
	if _, err := s.settings.update(func(v *Settings) { v.HomesSnapshots = pol }); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan pengaturan")
		return
	}
	warning := s.syncAfterChange(r)
	s.record(r, actor, "snapshots.policy", warning == "", map[string]string{"homes": "all", "policy": policyText(pol)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
}

// snapshotTarget: target dari permintaan harus shared folder di panel, atau
// folder pribadi (pengguna yang ada, atau folder sisa) di pool itu.
func (s *Server) snapshotTarget(w http.ResponseWriter, r *http.Request, t proto.SnapshotTarget) bool {
	switch {
	case t.Share != "" && t.User == "":
		if sh, ok := s.shares.get(t.Share); ok && sh.Pool == t.Pool {
			return true
		}
	case t.User != "" && t.Share == "":
		homes, err := s.homesStatus(r)
		if err != nil {
			s.agentError(w, proto.OpHomesStatus, err)
			return false
		}
		for _, h := range homes {
			if h.User == t.User && h.Pool == t.Pool {
				return true
			}
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "folder tidak ditemukan")
	return false
}

func (s *Server) handleSnapshotsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	t := proto.SnapshotTarget{Pool: q.Get("pool"), Share: q.Get("share"), User: q.Get("user")}
	if !s.snapshotTarget(w, r, t) {
		return
	}
	var out proto.SnapshotsListResult
	if err := s.agent.Call(r.Context(), proto.OpSnapshotsList, t, &out); err != nil {
		s.agentError(w, proto.OpSnapshotsList, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSnapshotCreate mengambil snapshot manual (tidak dihapus otomatis).
func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	var t proto.SnapshotTarget
	if !decodeJSON(w, r, &t) {
		return
	}
	if !s.snapshotTarget(w, r, t) {
		return
	}
	var out map[string]string
	err := s.poolCall(r.Context(), proto.OpSnapshotsCreate, t, &out)
	sess, _, _ := s.currentSession(r)
	detail := map[string]string{"pool": auditText(t.Pool), "share": auditText(t.Share), "user": auditText(t.User), "name": out["name"]}
	if err != nil {
		detail["error"] = errorCode(err)
	}
	s.record(r, sess.Username, "snapshots.create", err == nil, detail)
	if err != nil {
		s.agentError(w, proto.OpSnapshotsCreate, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSnapshotDelete menghapus satu snapshot: destruktif (versi lama
// hilang), butuh password admin dan tercatat di log audit.
func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		proto.SnapshotParams
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "nama snapshot wajib diisi")
		return
	}
	if !s.snapshotTarget(w, r, req.SnapshotTarget) {
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	s.destructive(w, r, admin, "snapshots.delete", map[string]string{"pool": auditText(req.Pool), "share": auditText(req.Share), "user": auditText(req.User), "name": auditText(req.Name)},
		proto.OpSnapshotsDelete, req.SnapshotParams, nil)
}
