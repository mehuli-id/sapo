package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/mehuli-id/sapo/internal/proto"
)

// --- Sapo Drive (ADR 0013) ---------------------------------------------------
//
// Setiap pengguna mendapat folder pribadi di pool yang dipilih admin
// (settings.json: homes_pool). Folder disiapkan sapod pada setiap
// sinkronisasi (homes.apply, keadaan lengkap) dan dibagikan lewat SMB
// ([homes]) serta File Station ("Drive saya"). Folder pengguna yang dihapus
// disimpan sampai admin menghapusnya sendiri.

// homeShare: nama folder pribadi di File Station (tidak mungkin bentrok
// dengan nama shared folder, yang selalu diawali huruf).
const homeShare = "@home"

const gib = uint64(1) << 30

// maxQuotaGB membatasi kuota (sekitar 1 EiB).
const maxQuotaGB = 1 << 30

// homeQuota: kuota pengguna dalam byte (0 = tanpa batas). Kuota per
// pengguna menimpa bawaan; -1 = tanpa batas.
func homeQuota(v Settings, user string) uint64 {
	q, ok := v.HomeQuotaGB[user]
	if !ok {
		q = v.HomesQuotaGB
	}
	if q <= 0 {
		return 0
	}
	return uint64(q) * gib
}

// homesStatus membaca semua folder pribadi di sistem. Sapod lama yang belum
// mengenal homes.status dianggap tidak punya folder pribadi.
func (s *Server) homesStatus(r *http.Request) ([]proto.HomeStatus, error) {
	var out []proto.HomeStatus
	err := s.agent.Call(r.Context(), proto.OpHomesStatus, nil, &out)
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == proto.ErrUnknownOp {
		return []proto.HomeStatus{}, nil
	}
	return out, err
}

// homeExists: masih ada folder pribadi atas nama user (di pool mana pun).
// Dipakai sebelum membuat pengguna baru: pengguna baru bernama sama tidak
// boleh mewarisi data pengguna lama.
func (s *Server) homeExists(r *http.Request, user string) (bool, error) {
	homes, err := s.homesStatus(r)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(homes, func(h proto.HomeStatus) bool { return h.User == user }), nil
}

// handleDrive: Sapo Drive untuk pengguna sesi ini.
func (s *Server) handleDrive(w http.ResponseWriter, r *http.Request) {
	sess, _, _ := s.currentSession(r)
	v := s.settings.get()
	out := map[string]any{"enabled": v.HomesPool != "", "pool": v.HomesPool, "smb": !v.SMBOff, "user": sess.Username}
	if v.HomesPool != "" {
		homes, err := s.homesStatus(r)
		if err != nil {
			s.agentError(w, proto.OpHomesStatus, err)
			return
		}
		for _, h := range homes {
			if h.User == sess.Username && h.Pool == v.HomesPool {
				out["home"] = h
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// driveUserView adalah satu baris tabel admin.
type driveUserView struct {
	User string `json:"user"`
	// Exists: pengguna masih ada di panel (false = folder sisa pengguna yang
	// dihapus).
	Exists bool `json:"exists"`
	// QuotaGB: kuota khusus (-1 = tanpa batas); tidak ada = bawaan.
	QuotaGB *int64            `json:"quota_gb,omitempty"`
	Home    *proto.HomeStatus `json:"home,omitempty"`
}

// handleDriveAdmin: pengaturan dan semua folder pribadi.
func (s *Server) handleDriveAdmin(w http.ResponseWriter, r *http.Request) {
	v := s.settings.get()
	homes, err := s.homesStatus(r)
	if err != nil {
		s.agentError(w, proto.OpHomesStatus, err)
		return
	}
	rows := []driveUserView{}
	seen := map[string]bool{}
	for _, u := range s.store.Users() {
		row := driveUserView{User: u.Username, Exists: true}
		if q, ok := v.HomeQuotaGB[u.Username]; ok {
			row.QuotaGB = &q
		}
		for i := range homes {
			if homes[i].User == u.Username && homes[i].Pool == v.HomesPool {
				row.Home = &homes[i]
			}
		}
		seen[u.Username] = true
		rows = append(rows, row)
	}
	// Folder sisa: pengguna sudah dihapus, atau folder di pool lain.
	orphans := []proto.HomeStatus{}
	for _, h := range homes {
		if !seen[h.User] || h.Pool != v.HomesPool {
			orphans = append(orphans, h)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pool": v.HomesPool, "quota_gb": v.HomesQuotaGB, "users": rows, "orphans": orphans})
}

// handleDriveSettings menyalakan Sapo Drive di pool (atau mematikannya:
// pool kosong, folder disimpan) dan mengatur kuota bawaan.
func (s *Server) handleDriveSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pool          string `json:"pool"`
		QuotaGB       int64  `json:"quota_gb"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if (req.Pool != "" && !shareNameRe.MatchString(req.Pool)) || req.QuotaGB < 0 || req.QuotaGB > maxQuotaGB {
		writeError(w, http.StatusBadRequest, "bad_request", "pool atau kuota tidak valid")
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	// Folder pribadi yang sudah ada tidak dipindah: pindah pool hanya bila
	// belum ada folder pribadi di pool lain (data dulu).
	if req.Pool != "" {
		homes, err := s.homesStatus(r)
		if err != nil {
			s.agentError(w, proto.OpHomesStatus, err)
			return
		}
		if i := slices.IndexFunc(homes, func(h proto.HomeStatus) bool { return h.Pool != req.Pool }); i >= 0 {
			writeErrorDetail(w, http.StatusConflict, "drive_pool_in_use", "masih ada folder pribadi di pool lain", map[string]any{"pool": homes[i].Pool})
			return
		}
	}
	if _, err := s.settings.update(func(v *Settings) { v.HomesPool, v.HomesQuotaGB = req.Pool, req.QuotaGB }); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan pengaturan")
		return
	}
	warning := s.syncAfterChange(r)
	s.record(r, admin.Username, "drive.settings", warning == "", map[string]string{"pool": auditText(req.Pool), "quota_gb": strconv.FormatInt(req.QuotaGB, 10)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
}

// handleDriveQuota mengatur kuota satu pengguna: quota_gb null = bawaan,
// -1 = tanpa batas.
func (s *Server) handleDriveQuota(w http.ResponseWriter, r *http.Request) {
	var req struct {
		QuotaGB       *int64 `json:"quota_gb"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	if _, ok := s.store.Get(name); !ok {
		writeError(w, http.StatusNotFound, "not_found", "user tidak ditemukan")
		return
	}
	if req.QuotaGB != nil && (*req.QuotaGB < -1 || *req.QuotaGB == 0 || *req.QuotaGB > maxQuotaGB) {
		writeError(w, http.StatusBadRequest, "bad_request", "kuota tidak valid")
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	_, err := s.settings.update(func(v *Settings) {
		m := map[string]int64{}
		for k, q := range v.HomeQuotaGB {
			m[k] = q
		}
		if req.QuotaGB == nil {
			delete(m, name)
		} else {
			m[name] = *req.QuotaGB
		}
		v.HomeQuotaGB = m
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "gagal menyimpan pengaturan")
		return
	}
	warning := s.syncAfterChange(r)
	q := "default"
	if req.QuotaGB != nil {
		q = strconv.FormatInt(*req.QuotaGB, 10)
	}
	s.record(r, admin.Username, "drive.quota", warning == "", map[string]string{"user": name, "quota_gb": q})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
}

// handleHomeDelete menghapus folder pribadi beserta isinya. Hanya untuk
// pengguna yang sudah dihapus dari panel; nama diketik ulang dan password
// admin, tercatat di log audit.
func (s *Server) handleHomeDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ConfirmName   string `json:"confirm_name"`
		AdminPassword string `json:"admin_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	pool, name := r.PathValue("pool"), r.PathValue("name")
	if _, exists := s.store.Get(name); exists {
		writeError(w, http.StatusConflict, "home_user_exists", "pengguna masih ada; hapus penggunanya dulu")
		return
	}
	if req.ConfirmName != name {
		writeErrorDetail(w, http.StatusBadRequest, "confirm_mismatch", "nama yang diketik ulang tidak sama", map[string]any{"field": "confirm_name"})
		return
	}
	admin, ok := s.reauthField(w, r, req.AdminPassword, "admin_password")
	if !ok {
		return
	}
	s.destructive(w, r, admin, "drive.home_delete", map[string]string{"user": auditText(name), "pool": auditText(pool)}, proto.OpHomesDelete,
		proto.HomeDeleteParams{Pool: pool, User: name}, nil)
}
