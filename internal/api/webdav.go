package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"github.com/mehuli-id/sapo/internal/agent"
	"github.com/mehuli-id/sapo/internal/auth"
	"github.com/mehuli-id/sapo/internal/proto"
)

// --- WebDAV (Sapo Drive tahap 2, ADR 0013) -----------------------------------
//
// https://<nas>/dav/ berisi folder pribadi pengguna (@home) dan shared
// folder yang boleh dibukanya, dengan izin yang sama dengan File Station.
// Setiap operasi diteruskan ke sapod dan dijalankan sebagai akun Linux
// pengguna itu (ADR 0010), jadi izinnya tetap ditegakkan kernel. Login
// memakai nama pengguna dan password aplikasi (HTTP Basic, hanya HTTPS);
// password akun dan 2FA tetap hanya untuk panel. Tidak ada cookie, jadi
// tidak ada CSRF.

const davPrefix = "/dav"

// davHomeName: folder pribadi di WebDAV (sama dengan File Station).
const davHomeName = homeShare

// davAuth memeriksa login WebDAV. Kegagalan dihitung limiter (per jaringan
// klien dan per akun, terpisah dari login panel) dan dicatat di log audit.
func (s *Server) davAuth(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	name, secret, hasAuth := r.BasicAuth()
	if !hasAuth {
		w.Header().Set("WWW-Authenticate", `Basic realm="Sapo Drive", charset="UTF-8"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "masuk dengan nama pengguna dan password aplikasi")
		return auth.User{}, false
	}
	// Hanya kegagalan yang dihitung (password aplikasi acak ~75 bit, jadi
	// permintaan bersamaan dari klien yang benar tidak pernah terkunci), dan
	// kunci akun dihitung per jaringan klien: orang lain tidak bisa mengunci
	// WebDAV pengguna dari jaringannya sendiri. Nama yang tidak ada diperlakukan
	// sama dengan yang ada.
	ipKey := "dav-ip:" + clientNet(r)
	userKey := "dav-user:" + clientNet(r) + ":" + name
	if rem := max(s.limiter.Locked(ipKey), s.limiter.Locked(userKey)); rem > 0 {
		s.writeLocked(w, rem)
		return auth.User{}, false
	}
	u, exists := s.store.Get(name)
	ok := false
	if exists && !u.Disabled && !u.MustChangePassword {
		_, ok = s.store.CheckAppPassword(name, secret, s.now())
	}
	if !ok {
		s.limiter.Begin(ipKey)
		s.limiter.Begin(userKey)
		actor := ""
		if exists {
			actor = u.Username
		}
		s.record(r, actor, "dav.login", false, map[string]string{"reason": "bad_credentials"})
		w.Header().Set("WWW-Authenticate", `Basic realm="Sapo Drive", charset="UTF-8"`)
		writeError(w, http.StatusUnauthorized, "bad_credentials", "nama pengguna atau password aplikasi salah")
		return auth.User{}, false
	}
	return u, true
}

// handleDAV melayani /dav/.
func (s *Server) handleDAV(w http.ResponseWriter, r *http.Request) {
	if s.settings.get().WebDAVOff {
		writeError(w, http.StatusForbidden, "webdav_off", "WebDAV dimatikan administrator")
		return
	}
	// Permintaan dari halaman web lain (browser yang menyimpan login Basic)
	// ditolak.
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
			writeError(w, http.StatusForbidden, "csrf", "permintaan ditolak")
			return
		}
	}
	// Password ada di setiap permintaan: hanya lewat HTTPS (HTTP biasa hanya
	// dari NAS sendiri, untuk mode pengembangan).
	if r.TLS == nil && !isLoopback(r) {
		writeError(w, http.StatusForbidden, "https_required", "WebDAV hanya lewat HTTPS")
		return
	}
	u, ok := s.davAuth(w, r)
	if !ok {
		return
	}
	// PROPFIND tanpa batas kedalaman (RFC 4918 §9.1) bisa memanggil sapod
	// untuk setiap direktori: hanya Depth 0 dan 1.
	if r.Method == "PROPFIND" && r.Header.Get("Depth") != "0" && r.Header.Get("Depth") != "1" {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>`)
		return
	}
	// Isi file tidak pernah ditampilkan sebagai halaman di origin panel.
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", "attachment")
	}
	k := &keepAlive{rc: http.NewResponseController(w)}
	k.touch()
	var in io.Reader = r.Body
	if r.Method != http.MethodPut {
		// Badan XML (PROPFIND, LOCK, PROPPATCH) kecil.
		in = http.MaxBytesReader(w, r.Body, 1<<20)
	}
	body := &davBody{r: deadlineReader{r: in, k: k}}
	r.Body = io.NopCloser(body)
	shares, all := s.sharesFor(r.Context(), u)
	req := &davRequest{body: body, put: r.Method == http.MethodPut, method: r.Method}
	fsys := &davFS{s: s, user: u.Username, shares: shares, all: all, req: req, stat: map[string]*davInfo{}}
	if t, ok := s.homeTargetFor(u); ok {
		fsys.home = &t
	}
	// Menulis ke folder baca saja ditolak dengan jelas (403) sebelum
	// diteruskan; webdav sendiri menjawab galat izin sebagai 404. sapod tetap
	// memeriksa ulang sebagai akun pengguna.
	if davWrites(r.Method) && !fsys.writable(strings.TrimPrefix(r.URL.Path, davPrefix)) {
		writeError(w, http.StatusForbidden, "permission_denied", "folder ini hanya bisa dibaca")
		return
	}
	if r.Method == "MOVE" || r.Method == "COPY" {
		d, err := url.Parse(r.Header.Get("Destination"))
		if err != nil || !strings.HasPrefix(d.Path, davPrefix+"/") || !fsys.writable(strings.TrimPrefix(d.Path, davPrefix)) {
			writeError(w, http.StatusForbidden, "permission_denied", "tujuan hanya bisa dibaca")
			return
		}
		src, dst := davKey(strings.TrimPrefix(r.URL.Path, davPrefix)), davKey(strings.TrimPrefix(d.Path, davPrefix))
		// Ke dalam dirinya sendiri: tidak pernah selesai.
		if strings.HasPrefix(dst+"/", src+"/") {
			writeError(w, http.StatusForbidden, "bad_request", "tujuan berada di dalam sumber")
			return
		}
		// Pindah antar-folder tidak didukung (sistem berkas berbeda): klien
		// menyalin lalu menghapus. Ditolak sebelum webdav sempat menghapus
		// tujuan yang akan ditimpa.
		if r.Method == "MOVE" && strings.SplitN(src, "/", 2)[0] != strings.SplitN(dst, "/", 2)[0] {
			writeError(w, http.StatusBadGateway, "cross_folder", "pindah antar-folder tidak didukung; salin lalu hapus")
			return
		}
		req.dest = dst
	}
	rec := &statusRecorder{ResponseWriter: davWriter{ResponseWriter: w, k: k}, status: http.StatusOK}
	handler := &webdav.Handler{Prefix: davPrefix, FileSystem: fsys, LockSystem: s.davLocks.forUser(u.Username)}
	if r.Method == "PROPFIND" {
		// Jawaban PROPFIND ditampung dulu: bila daftar isi gagal (misalnya
		// terpotong), klien mendapat galat utuh, bukan daftar setengah jadi.
		buf := &davBuffer{h: http.Header{}, status: http.StatusOK}
		handler.ServeHTTP(buf, r)
		if req.listFailed {
			writeError(rec, http.StatusInsufficientStorage, "too_many_files", "folder berisi terlalu banyak file untuk WebDAV")
			return
		}
		for k, v := range buf.h {
			h[k] = v
		}
		rec.WriteHeader(buf.status)
		_, _ = rec.Write(buf.b.Bytes())
		return
	}
	handler.ServeHTTP(rec, r)
	switch r.Method {
	case http.MethodPut, http.MethodDelete, "MKCOL", "MOVE", "COPY", "LOCK", "UNLOCK", "PROPPATCH":
		detail := map[string]string{"path": auditText(strings.TrimPrefix(r.URL.Path, davPrefix)), "status": strconv.Itoa(rec.status)}
		if d := r.Header.Get("Destination"); d != "" {
			detail["to"] = auditText(d)
			// Bawaan webdav: COPY menimpa kecuali "F", MOVE hanya bila "T".
			ow := r.Header.Get("Overwrite") != "F"
			if r.Method == "MOVE" {
				ow = r.Header.Get("Overwrite") == "T"
			}
			detail["overwrite"] = strconv.FormatBool(ow)
		}
		s.record(r, u.Username, "dav."+strings.ToLower(r.Method), rec.status < 300, detail)
	}
}

// davWrites: metode yang mengubah isi folder.
func davWrites(m string) bool {
	switch m {
	case http.MethodPut, http.MethodDelete, "MKCOL", "MOVE", "PROPPATCH", "LOCK":
		return true
	}
	return false
}

// writable: path berada di folder yang boleh ditulis (bukan akar /dav/ dan
// bukan akar folder itu sendiri). Folder yang tidak ada dianggap boleh agar
// webdav menjawab 404 seperti biasa.
func (f *davFS) writable(name string) bool {
	p, err := f.resolve(name)
	if err != nil {
		return true
	}
	return p.share != "" && p.path != "" && p.write
}

// homeTargetFor: folder pribadi u bila Sapo Drive menyala.
func (s *Server) homeTargetFor(u auth.User) (proto.FilesTarget, bool) {
	pool := s.settings.get().HomesPool
	if pool == "" || u.Disabled {
		return proto.FilesTarget{}, false
	}
	return proto.FilesTarget{User: u.Username, Pool: pool, Home: true}, true
}

// davWriter memperpanjang batas waktu koneksi selama unduhan mengalir.
// Flush diteruskan agar jawaban PROPFIND besar tetap mengalir.
type davWriter struct {
	http.ResponseWriter
	k *keepAlive
}

func (d davWriter) Write(p []byte) (int, error) {
	d.k.touch()
	return d.ResponseWriter.Write(p)
}

func (d davWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }

// davBody mencatat apakah badan permintaan terbaca utuh: unggahan yang
// terputus tidak boleh menggantikan file lama.
type davBody struct {
	r    io.Reader
	mu   sync.Mutex
	eof  bool
	fail bool
}

func (b *davBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case errors.Is(err, io.EOF):
		b.eof = true
	case err != nil:
		b.fail = true
	}
	return n, err
}

func (b *davBody) complete() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.eof && !b.fail
}

// davRequest: keadaan satu permintaan WebDAV.
type davRequest struct {
	body   *davBody
	put    bool
	method string
	// dest: tujuan MOVE/COPY (davKey). Penghapusan tujuan sebelum ditimpa
	// ditunda (deferred) agar penimpaan atomik (rename atau commit).
	dest     string
	deferred bool
	// listFailed: daftar isi direktori tidak lengkap.
	listFailed bool
	mu         sync.Mutex
	// readFail: membaca file sumber gagal (COPY), jadi salinannya tidak
	// diberi nama akhir.
	readFail bool
}

func (q *davRequest) markReadFail() {
	q.mu.Lock()
	q.readFail = true
	q.mu.Unlock()
}

func (q *davRequest) writeOK() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.readFail {
		return false
	}
	return !q.put || q.body.complete()
}

// davFS memetakan /<folder>/<path> ke operasi file sapod sebagai user.
type davFS struct {
	s      *Server
	user   string
	shares map[string]fileShare
	all    map[string]Share
	home   *proto.FilesTarget
	req    *davRequest
	// stat: info yang sudah diketahui di permintaan ini (dari daftar isi
	// direktori), agar PROPFIND tidak memanggil sapod untuk setiap file.
	statMu sync.Mutex
	stat   map[string]*davInfo
}

// davPath: nama dari webdav (diawali "/") → folder, path, dan apakah
// folder itu boleh ditulis. folder "" = akar /dav/.
type davPath struct {
	share  string
	path   string
	target proto.FilesTarget
	write  bool
}

func (f *davFS) resolve(name string) (davPath, error) {
	name = strings.Trim(path.Clean("/"+name), "/")
	if name == "" {
		return davPath{}, nil
	}
	share, rest, _ := strings.Cut(name, "/")
	clean, err := agent.CleanFilePath(rest)
	if err != nil {
		return davPath{}, fs.ErrNotExist
	}
	p := davPath{share: share, path: clean}
	switch {
	case share == davHomeName && f.home != nil:
		p.target, p.write = *f.home, true
	default:
		sh, ok := f.shares[share]
		if !ok {
			return davPath{}, fs.ErrNotExist
		}
		p.target = proto.FilesTarget{User: f.user, Pool: f.all[share].Pool, Share: share}
		if sh.External {
			p.target = proto.FilesTarget{User: f.user, Share: share, External: true}
		}
		p.write = sh.Access == "write"
	}
	p.target.Path = clean
	return p, nil
}

// davError menerjemahkan galat sapod ke galat yang dikenal webdav (404,
// 403, 405/412).
func davError(err error) error {
	var pe *proto.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case proto.ErrNotFound:
			return fs.ErrNotExist
		case "permission_denied":
			return fs.ErrPermission
		case "file_exists", "not_empty":
			return fs.ErrExist
		}
	}
	return err
}

func (f *davFS) call(ctx context.Context, op string, params, out any) error {
	return davError(f.s.agent.Call(ctx, op, params, out))
}

func (f *davFS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	p, err := f.resolve(name)
	switch {
	case err != nil:
		return err
	case p.share == "" || p.path == "":
		return fs.ErrExist
	case !p.write:
		return fs.ErrPermission
	}
	f.forget(name)
	return f.call(ctx, proto.OpFilesMkdir, p.target, nil)
}

func (f *davFS) RemoveAll(ctx context.Context, name string) error {
	p, err := f.resolve(name)
	switch {
	case err != nil:
		return err
	case p.share == "" || p.path == "":
		return fs.ErrPermission
	case !p.write:
		return fs.ErrPermission
	}
	// MOVE/COPY yang menimpa file: tujuan tidak dihapus dulu, melainkan
	// ditimpa atomik oleh Rename atau Close. Bila operasinya gagal, file
	// lama tetap ada. Tujuan berupa direktori: COPY gagal (direktori tidak
	// ditimpa), MOVE menghapusnya dulu seperti biasa.
	if (f.req.method == "MOVE" || f.req.method == "COPY") && davKey(name) == f.req.dest {
		if info, err := f.Stat(ctx, name); err == nil && (!info.IsDir() || f.req.method == "COPY") {
			f.req.deferred = true
			return nil
		}
	}
	f.forget(name)
	return f.call(ctx, proto.OpFilesDelete, p.target, nil)
}

// davKey: nama yang sudah dibersihkan, tanpa "/" di depan.
func davKey(name string) string { return strings.Trim(path.Clean("/"+name), "/") }

func (f *davFS) Rename(ctx context.Context, oldName, newName string) error {
	from, err := f.resolve(oldName)
	if err != nil {
		return err
	}
	to, err := f.resolve(newName)
	if err != nil {
		return err
	}
	// Hanya di dalam satu folder (antar-folder = salin lalu hapus, oleh
	// klien).
	if from.share == "" || from.path == "" || to.path == "" || from.share != to.share || !from.write {
		return fs.ErrPermission
	}
	f.forget(oldName)
	f.forget(newName)
	overwrite := f.req.deferred && davKey(newName) == f.req.dest
	return f.call(ctx, proto.OpFilesRename, proto.FilesRenameParams{FilesTarget: from.target, To: to.path, Overwrite: overwrite}, nil)
}

func (f *davFS) forget(name string) {
	f.statMu.Lock()
	delete(f.stat, strings.Trim(path.Clean("/"+name), "/"))
	f.statMu.Unlock()
}

func (f *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	key := strings.Trim(path.Clean("/"+name), "/")
	f.statMu.Lock()
	if in, ok := f.stat[key]; ok {
		f.statMu.Unlock()
		return in, nil
	}
	f.statMu.Unlock()
	p, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	if p.share == "" {
		return &davInfo{name: "/", dir: true}, nil
	}
	var e proto.FileEntry
	if err := f.call(ctx, proto.OpFilesStat, p.target, &e); err != nil {
		return nil, err
	}
	if e.Type != "dir" && e.Type != "file" {
		// Symlink dan berkas khusus tidak pernah diikuti.
		return nil, fs.ErrNotExist
	}
	in := &davInfo{name: path.Base("/" + key), size: e.Size, mtime: e.ModTime, dir: e.Type == "dir"}
	f.statMu.Lock()
	f.stat[key] = in
	f.statMu.Unlock()
	return in, nil
}

func (f *davFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	p, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		return f.openWrite(ctx, name, p, flag)
	}
	info, err := f.Stat(ctx, name)
	if err != nil {
		return nil, err
	}
	return &davFile{fs: f, ctx: ctx, name: name, p: p, info: info.(*davInfo)}, nil
}

// openWrite membuka file sementara di direktori induk; namanya baru
// diberikan saat Close, dan hanya bila isinya diterima utuh.
func (f *davFS) openWrite(ctx context.Context, name string, p davPath, flag int) (webdav.File, error) {
	switch {
	case p.share == "" || p.path == "":
		return nil, fs.ErrPermission
	case !p.write:
		return nil, fs.ErrPermission
	case flag&os.O_APPEND != 0:
		return nil, fs.ErrPermission
	}
	if flag&os.O_EXCL != 0 {
		if _, err := f.Stat(ctx, name); err == nil {
			return nil, fs.ErrExist
		}
	}
	dir := p.target
	dir.Path = path.Dir("/" + p.path)[1:]
	var info proto.FilesOpenResult
	file, err := f.s.agent.CallFile(ctx, proto.OpFilesOpenWrite, dir, &info)
	if err != nil {
		return nil, davError(err)
	}
	f.forget(name)
	return &davFile{fs: f, ctx: ctx, name: name, p: p, w: file, temp: info.Temp,
		info: &davInfo{name: path.Base(p.path), mtime: time.Now()}}, nil
}

// davInfo adalah os.FileInfo untuk WebDAV.
type davInfo struct {
	name  string
	size  int64
	mtime time.Time
	dir   bool
}

func (i *davInfo) Name() string { return i.name }
func (i *davInfo) Size() int64  { return i.size }
func (i *davInfo) Mode() os.FileMode {
	if i.dir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
func (i *davInfo) ModTime() time.Time { return i.mtime }
func (i *davInfo) IsDir() bool        { return i.dir }
func (i *davInfo) Sys() any           { return nil }

// ContentType dari ekstensi saja (webdav.ContentTyper): isi file tidak
// dibaca hanya untuk PROPFIND.
func (i *davInfo) ContentType(context.Context) (string, error) {
	if t := mime.TypeByExtension(path.Ext(i.name)); t != "" {
		return t, nil
	}
	return "application/octet-stream", nil
}

// davFile adalah direktori, file baca (dibuka saat pertama dibaca), atau
// file tulis (file sementara).
type davFile struct {
	fs   *davFS
	ctx  context.Context
	name string
	p    davPath
	info *davInfo
	// Baca.
	r *os.File
	// Tulis.
	w        *os.File
	temp     string
	writeErr bool
	wrote    bool
	closed   bool
}

func (d *davFile) open() error {
	if d.r != nil {
		return nil
	}
	if d.info.dir {
		return fs.ErrInvalid
	}
	var info proto.FilesOpenResult
	file, err := d.fs.s.agent.CallFile(d.ctx, proto.OpFilesOpenRead, d.p.target, &info)
	if err != nil {
		return davError(err)
	}
	d.r = file
	return nil
}

func (d *davFile) Read(p []byte) (int, error) {
	if d.w != nil {
		return 0, fs.ErrInvalid
	}
	if err := d.open(); err != nil {
		d.fs.req.markReadFail()
		return 0, err
	}
	n, err := d.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		d.fs.req.markReadFail()
	}
	return n, err
}

func (d *davFile) Seek(offset int64, whence int) (int64, error) {
	if d.w != nil {
		return d.w.Seek(offset, whence)
	}
	if err := d.open(); err != nil {
		return 0, err
	}
	return d.r.Seek(offset, whence)
}

func (d *davFile) Write(p []byte) (int, error) {
	if d.w == nil {
		return 0, fs.ErrPermission
	}
	n, err := d.w.Write(p)
	if n > 0 {
		d.wrote = true
	}
	if err != nil {
		d.writeErr = true
	}
	return n, err
}

func (d *davFile) Readdir(count int) ([]fs.FileInfo, error) {
	if !d.info.dir {
		return nil, fs.ErrInvalid
	}
	var out []fs.FileInfo
	base := strings.Trim(path.Clean("/"+d.name), "/")
	if d.p.share == "" {
		// Akar: folder pribadi dan shared folder yang boleh dibuka.
		if d.fs.home != nil {
			out = append(out, &davInfo{name: davHomeName, dir: true})
		}
		for name := range d.fs.shares {
			out = append(out, &davInfo{name: name, dir: true})
		}
	} else {
		var list proto.FilesListResult
		if err := d.fs.call(d.ctx, proto.OpFilesList, d.p.target, &list); err != nil {
			return nil, err
		}
		// Daftar yang terpotong tidak pernah diberikan sebagai daftar
		// lengkap (klien sinkron akan menganggap sisanya terhapus, COPY
		// akan kehilangan file).
		if list.Truncated {
			d.fs.req.listFailed = true
			return nil, errors.New("folder berisi terlalu banyak file untuk WebDAV")
		}
		d.fs.statMu.Lock()
		for _, e := range list.Entries {
			if e.Type != "dir" && e.Type != "file" {
				continue
			}
			in := &davInfo{name: e.Name, size: e.Size, mtime: e.ModTime, dir: e.Type == "dir"}
			d.fs.stat[strings.TrimPrefix(base+"/"+e.Name, "/")] = in
			out = append(out, in)
		}
		d.fs.statMu.Unlock()
	}
	if count > 0 && len(out) > count {
		out = out[:count]
	}
	return out, nil
}

func (d *davFile) Stat() (fs.FileInfo, error) {
	if d.w != nil {
		st, err := d.w.Stat()
		if err != nil {
			return nil, err
		}
		d.info.size, d.info.mtime = st.Size(), st.ModTime()
	}
	return d.info, nil
}

// Close: file tulis diberi nama akhir (menimpa file lama) hanya bila
// isinya diterima utuh; bila tidak, file sementara dibuang.
func (d *davFile) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	if d.r != nil {
		return d.r.Close()
	}
	if d.w == nil {
		return nil
	}
	closeErr := d.w.Close()
	ctx := context.WithoutCancel(d.ctx)
	// Menimpa file lama hanya untuk PUT, atau salinan (COPY) yang memang
	// berisi data. File yang dibuat tanpa isi (misalnya LOCK pada nama yang
	// gagal dibaca karena sapod sibuk) tidak pernah menggantikan file lama.
	overwrite := d.fs.req.put || d.wrote || (d.fs.req.deferred && davKey(d.name) == d.fs.req.dest)
	commit := proto.FilesCommitParams{FilesTarget: d.p.target, Temp: d.temp, Overwrite: overwrite}
	if closeErr != nil || d.writeErr || !d.fs.req.writeOK() {
		_ = d.fs.s.agent.Call(ctx, proto.OpFilesAbort, commit, nil)
		return errors.Join(closeErr, errors.New("unggahan tidak lengkap"))
	}
	if err := d.fs.call(ctx, proto.OpFilesCommit, commit, nil); err != nil {
		_ = d.fs.s.agent.Call(ctx, proto.OpFilesAbort, commit, nil)
		if !overwrite && errors.Is(err, fs.ErrExist) {
			// Sudah ada: dibiarkan apa adanya.
			return nil
		}
		return err
	}
	return nil
}

// isLoopback: permintaan dari NAS sendiri (mode pengembangan tanpa HTTPS).
func isLoopback(r *http.Request) bool {
	addr, err := netip.ParseAddr(clientIP(r))
	return err == nil && addr.Unmap().IsLoopback()
}

// davBuffer menampung satu jawaban (PROPFIND).
type davBuffer struct {
	h      http.Header
	status int
	b      bytes.Buffer
}

func (d *davBuffer) Header() http.Header         { return d.h }
func (d *davBuffer) Write(p []byte) (int, error) { return d.b.Write(p) }
func (d *davBuffer) WriteHeader(code int)        { d.status = code }
