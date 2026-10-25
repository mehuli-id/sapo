package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mehuli-id/sapo/internal/proto"
)

// fakeSpawn: "zfs send" menulis nama snapshot ke pipa; "zfs receive" membaca
// nama itu lalu membuat dataset/snapshot di fakeZFS.
func fakeSpawn(f *fakeZFS) func(ctx context.Context, args []string, in bool) (*os.File, func() error, error) {
	return func(ctx context.Context, args []string, in bool) (*os.File, func() error, error) {
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, nil, err
		}
		f.mu.Lock()
		f.calls = append(f.calls, strings.Join(args, " "))
		f.mu.Unlock()
		done := make(chan error, 1)
		if in {
			go func() {
				data, _ := io.ReadAll(pr)
				pr.Close()
				if len(data) <= drrBeginLen {
					done <- errors.New("aliran kosong")
					return
				}
				src := string(data[drrBeginLen:])
				// Target ds@snap: nama snapshot dari penerima.
				dst, snap, _ := strings.Cut(args[len(args)-1], "@")
				f.mu.Lock()
				if _, ok := f.ds[dst]; !ok {
					props := map[string]string{"type": "filesystem"}
					for i := 0; i+1 < len(args); i++ {
						if args[i] == "-o" {
							k, v, _ := strings.Cut(args[i+1], "=")
							props[k] = v
						}
					}
					f.ds[dst] = props
				}
				f.snaps[dst+"@"+snap] = f.snaps[src]
				f.mu.Unlock()
				done <- nil
			}()
			return pw, func() error { return <-done }, nil
		}
		incremental := slices.Contains(args, "-i")
		go func() {
			_, err := pw.Write(append(fakeHeader(incremental, dmuSubstream, 0, 0), args[len(args)-1]...))
			pw.Close()
			done <- err
		}()
		return pr, func() error { return <-done }, nil
	}
}

func rbCall(t *testing.T, h Handler, p any) (any, error) {
	t.Helper()
	raw, _ := json.Marshal(p)
	return h(context.Background(), raw)
}

// transfer: satu backup lewat "jaringan" (salin file descriptor), seperti
// yang dilakukan sapo-api di kedua NAS.
func transfer(t *testing.T, r *RemoteBackups, remote []string, keep int) (string, error) {
	t.Helper()
	res, err := rbCall(t, r.SendOpenOp(), proto.RBackupSendParams{Pool: "tank", Share: "keluarga", Job: "kel", Remote: remote})
	if err != nil {
		return "", err
	}
	sfr := res.(FileResult)
	ss := sfr.Result.(proto.RBackupSession)
	tgt := proto.RBackupTarget{Pool: "usb", Partner: "abcdef0123", Job: "kel"}
	res, err = rbCall(t, r.RecvOpenOp(), proto.RBackupRecvParams{RBackupTarget: tgt, Base: ss.Base, Snap: ss.Snap})
	if err != nil {
		sfr.File.Close()
		_, _ = rbCall(t, r.SendFinishOp(), proto.RBackupSendFinish{Session: ss.Session, OK: false})
		return "", err
	}
	rfr := res.(FileResult)
	_, copyErr := io.Copy(rfr.File, sfr.File)
	rfr.File.Close()
	sfr.File.Close()
	rs := rfr.Result.(proto.RBackupSession)
	_, recvErr := rbCall(t, r.RecvFinishOp(), proto.RBackupRecvFinish{Session: rs.Session, OK: copyErr == nil, KeepDays: keep})
	if _, err := rbCall(t, r.SendFinishOp(), proto.RBackupSendFinish{Session: ss.Session, OK: copyErr == nil, RemoteHas: recvErr == nil}); err != nil {
		return "", err
	}
	return ss.Snap, recvErr
}

func remoteCopies(t *testing.T, r *RemoteBackups) []string {
	t.Helper()
	res, err := rbCall(t, r.ListOp(), proto.RBackupTarget{Pool: "usb", Partner: "abcdef0123", Job: "kel"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range res.(proto.RBackupList).Copies {
		names = append(names, c.Name)
	}
	return names
}

func TestRemoteBackupRoundTrip(t *testing.T) {
	b, f := testBackups(t)
	r := NewRemoteBackups(b)
	r.spawn = fakeSpawn(f)
	var oe *OpError

	// Validasi nama: tidak bisa keluar dari dataset partner.
	for _, bad := range []proto.RBackupRecvParams{
		{RBackupTarget: proto.RBackupTarget{Pool: "usb", Partner: "../x", Job: "kel"}, Snap: rbackupPrefix("kel") + "2026.10.28-01.00.00"},
		{RBackupTarget: proto.RBackupTarget{Pool: "usb", Partner: "abcdef0123", Job: "kel/x"}, Snap: rbackupPrefix("kel") + "2026.10.28-01.00.00"},
		{RBackupTarget: proto.RBackupTarget{Pool: "usb", Partner: "abcdef0123", Job: "kel"}, Snap: "sapo-daily_GMT-2026.10.28-01.00.00"},
		{RBackupTarget: proto.RBackupTarget{Pool: "usb", Partner: "abcdef0123", Job: "kel"}, Snap: rbackupPrefix("kel") + "x@y"},
	} {
		if _, err := rbCall(t, r.RecvOpenOp(), bad); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
			t.Errorf("%+v harus ditolak: %v", bad, err)
		}
	}

	first, err := transfer(t, r, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if p := f.ds["usb/sapo-backup-remote/abcdef0123/kel"]; p == nil || p["readonly"] != "on" || p["canmount"] != "noauto" || p[backupPropName] != "remote:abcdef0123/kel" {
		t.Fatalf("salinan di penerima = %+v", p)
	}
	// Penuh lagi padahal penerima sudah punya salinan: ditolak.
	if _, err := transfer(t, r, nil, 2); !errors.As(err, &oe) || oe.Code != "backup_diverged" {
		t.Fatalf("penuh ke tujuan yang sudah ada: %v", err)
	}
	// Inkremental dua kali; penerima menyimpan 2.
	for i := 0; i < 2; i++ {
		if _, err := transfer(t, r, remoteCopies(t, r), 2); err != nil {
			t.Fatalf("inkremental %d: %v", i, err)
		}
	}
	// Salinan yang diterima kurang dari `keep` hari lalu tidak dibuang.
	have := remoteCopies(t, r)
	if len(have) != 3 || have[0] != first {
		t.Fatalf("salinan di penerima: %v", have)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "-x mountpoint -x sharenfs -x sharesmb -x setuid -x exec -x devices -- usb/sapo-backup-remote/abcdef0123/kel@"+rbackupPrefix("kel")) {
		t.Fatalf("receive tanpa pelindung properti:\n%s", strings.Join(f.calls, "\n"))
	}
	all := strings.Join(f.calls, "\n")
	if strings.Count(all, "send -L -c -i @"+rbackupPrefix("kel")) != 2 || strings.Contains(all, "receive -u -F") {
		t.Fatalf("panggilan:\n%s", all)
	}
	src := 0
	for k := range f.snaps {
		if strings.HasPrefix(k, "tank/keluarga@"+rbackupPrefix("kel")) {
			src++
		}
	}
	if src != 1 {
		t.Fatalf("sumber hanya menyimpan dasar berikutnya: %d %v", src, f.snaps)
	}
	// Dasar yang tidak lagi terbaru di penerima ditolak penerima.
	if _, err := rbCall(t, r.RecvOpenOp(), proto.RBackupRecvParams{RBackupTarget: proto.RBackupTarget{Pool: "usb", Partner: "abcdef0123", Job: "kel"}, Base: have[0], Snap: rbackupPrefix("kel") + "2030.01.01-00.00.00"}); !errors.As(err, &oe) || oe.Code != "backup_diverged" {
		t.Fatalf("dasar lama: %v", err)
	}
	// Pengirim kehilangan dasar: berhenti tanpa snapshot baru.
	before := len(f.snaps)
	if _, err := rbCall(t, r.SendOpenOp(), proto.RBackupSendParams{Pool: "tank", Share: "keluarga", Job: "kel", Remote: []string{rbackupPrefix("kel") + "2031.01.01-00.00.00"}}); !errors.As(err, &oe) || oe.Code != "backup_diverged" {
		t.Fatalf("dasar hilang di sumber: %v", err)
	}
	if len(f.snaps) != before {
		t.Fatal("tidak boleh membuat snapshot saat divergen")
	}
	// Pool penerima tidak bisa dihapus selama ada salinan partner.
	f.mu.Lock()
	f.ds["usb/sapo-backup-remote"] = map[string]string{"type": "filesystem", backupPropName: parentMarker}
	f.mu.Unlock()
	// Pulihkan di penerima sebagai folder baru.
	if _, err := rbCall(t, r.RestoreOp(), proto.RBackupRestoreParams{RBackupTarget: proto.RBackupTarget{Pool: "usb", Partner: "abcdef0123", Job: "kel"}, Copy: have[1], Name: "keluarga"}); err != nil {
		t.Fatal(err)
	}
	b.runMu.Lock()
	b.runMu.Unlock()
	if p := f.ds["usb/keluarga"]; p == nil || p[sharePropName] != "keluarga" {
		t.Fatalf("folder pulihan di penerima = %+v", p)
	}
	// Hapus semua salinan partner.
	if _, err := rbCall(t, r.DestroyOp(), proto.RBackupTarget{Pool: "usb", Partner: "abcdef0123"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "destroy -r usb/sapo-backup-remote/abcdef0123") {
		t.Fatalf("hapus partner:\n%s", strings.Join(f.calls, "\n"))
	}
}

// fakeHeader: rekaman DRR_BEGIN (little endian).
func fakeHeader(incremental bool, hdrType, features uint64, flags uint32) []byte {
	h := make([]byte, drrBeginLen)
	le := binary.LittleEndian
	le.PutUint64(h[8:], dmuBackupMagic)
	le.PutUint64(h[16:], features<<2|hdrType)
	le.PutUint32(h[32:], dmuOstZFS)
	le.PutUint32(h[36:], flags)
	le.PutUint64(h[40:], 7)
	if incremental {
		le.PutUint64(h[48:], 9)
	}
	return h
}

func TestCheckStreamHeader(t *testing.T) {
	ok := fakeHeader(false, dmuSubstream, 0, 0)
	if _, err := checkStreamHeader(strings.NewReader(string(ok)), false); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, drrBeginLen)
	copy(big, ok)
	be := binary.BigEndian
	be.PutUint64(big[8:], dmuBackupMagic)
	be.PutUint64(big[16:], dmuSubstream)
	be.PutUint32(big[32:], dmuOstZFS)
	be.PutUint32(big[36:], 0)
	be.PutUint64(big[48:], 0)
	if _, err := checkStreamHeader(strings.NewReader(string(big)), false); err != nil {
		t.Fatalf("big endian: %v", err)
	}
	zvol := fakeHeader(false, dmuSubstream, 0, 0)
	binary.LittleEndian.PutUint32(zvol[32:], 3)
	for name, c := range map[string]struct {
		h    []byte
		incr bool
	}{
		"gabungan (-R/-p)": {fakeHeader(false, 2, 0, 0), false},
		"raw":              {fakeHeader(false, dmuSubstream, backupFeatureRaw, 0), false},
		"clone":            {fakeHeader(false, dmuSubstream, 0, drrFlagClone), false},
		"zvol":             {zvol, false},
		"inkremental":      {fakeHeader(true, dmuSubstream, 0, 0), false},
		"bukan penuh":      {fakeHeader(false, dmuSubstream, 0, 0), true},
		"pendek":           {ok[:100], false},
		"sampah":           {make([]byte, drrBeginLen), false},
	} {
		if _, err := checkStreamHeader(strings.NewReader(string(c.h)), c.incr); err == nil {
			t.Errorf("%s harus ditolak", name)
		}
	}
}

// Stream gabungan dari partner jahat: zfs receive tidak pernah dijalankan.
func TestRemoteBackupRejectsCompoundStream(t *testing.T) {
	b, f := testBackups(t)
	r := NewRemoteBackups(b)
	r.spawn = fakeSpawn(f)
	tgt := proto.RBackupTarget{Pool: "usb", Partner: "abcdef0123", Job: "kel"}
	res, err := rbCall(t, r.RecvOpenOp(), proto.RBackupRecvParams{RBackupTarget: tgt, Snap: rbackupPrefix("kel") + "2026.10.28-01.00.00"})
	if err != nil {
		t.Fatal(err)
	}
	fr := res.(FileResult)
	_, _ = fr.File.Write(append(fakeHeader(false, 2, 0, 0), "usb/jahat@x"...))
	fr.File.Close()
	if _, err := rbCall(t, r.RecvFinishOp(), proto.RBackupRecvFinish{Session: fr.Result.(proto.RBackupSession).Session, OK: true, KeepDays: 3}); err == nil {
		t.Fatal("stream gabungan harus gagal")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "receive") {
			t.Fatalf("zfs receive tidak boleh dijalankan: %v", f.calls)
		}
	}
	if _, ok := f.ds["usb/sapo-backup-remote/abcdef0123/kel"]; ok {
		t.Fatal("dataset tidak boleh dibuat")
	}
}
