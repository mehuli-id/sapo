package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Time Machine untuk Mac (ADR 0022). Folder tujuan diberi kuota ZFS
// (refquota, agar snapshot tidak dihitung) yang terlihat oleh Mac sebagai
// ukuran disk, sehingga Time Machine membuang backup lama sendiri saat
// penuh. Samba (vfs_fruit, fruit:time machine) mengumumkan folder itu lewat
// Avahi (_adisk._tcp). Properti sapo:tm mencatat kuota yang dipasang Sapo,
// agar hanya kuota itu yang dilepas saat Time Machine dimatikan.

const tmPropName = "sapo:tm"

// setTimeMachine menyamakan kuota dataset dengan tm (nil = bukan tujuan
// Time Machine).
func (s *Shares) setTimeMachine(ctx context.Context, ds string, tm *proto.TimeMachine) error {
	if tm != nil && (tm.QuotaGB < proto.MinTimeMachineGB || tm.QuotaGB > proto.MaxTimeMachineGB) {
		return errors.New("kuota Time Machine tidak valid")
	}
	props, err := s.zfsProps(ctx, ds, tmPropName, "refquota", "xattr")
	if err != nil {
		return err
	}
	set := func(kv string) error {
		res, err := s.p.st.run(ctx, s.t.Zfs, []string{"set", kv}, ds)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("zfs set %s: %s", kv, firstLine(res.Stderr))
		}
		return nil
	}
	cur := props[tmPropName]
	if tm == nil {
		// Kuota dilepas hanya bila dipasang Sapo untuk Time Machine.
		if cur == "" || cur == "-" || cur == "off" {
			return nil
		}
		if err := set("refquota=none"); err != nil {
			return err
		}
		return set(tmPropName + "=off")
	}
	quota := strconv.FormatUint(uint64(tm.QuotaGB)<<30, 10)
	if props["refquota"] != quota {
		// Gagal bila kuota lebih kecil dari isi folder (zfs menolak).
		if err := set("refquota=" + quota); err != nil {
			return err
		}
	}
	if cur != strconv.Itoa(tm.QuotaGB) {
		if err := set(tmPropName + "=" + strconv.Itoa(tm.QuotaGB)); err != nil {
			return err
		}
	}
	// Atribut tambahan (metadata Mac) di dnode: jauh lebih cepat untuk
	// vfs_fruit/streams_xattr; hanya berlaku untuk atribut baru.
	if props["xattr"] != "sa" {
		return set("xattr=sa")
	}
	return nil
}
