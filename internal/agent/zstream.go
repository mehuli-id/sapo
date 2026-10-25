package agent

import (
	"encoding/binary"
	"errors"
	"io"
)

// Pemeriksa header stream zfs send dari NAS lain (ADR 0019). NAS pengirim
// yang jahat bisa membuat stream sendiri, jadi sebelum diteruskan ke zfs
// receive, rekaman DRR_BEGIN pertama harus:
//   - stream tunggal (substream): bukan stream gabungan zfs send -R/-p, yang
//     bisa membawa dataset anak dan properti (mountpoint, setuid, sharenfs);
//   - filesystem (bukan zvol), bukan clone, bukan raw/redacted;
//   - penuh atau inkremental sesuai yang diminta.
//
// Konstanta dari OpenZFS include/sys/zfs_ioctl.h.

const (
	drrBeginLen         = 312 // sizeof(dmu_replay_record_t)
	dmuBackupMagic      = 0x2F5bacbac
	dmuSubstream        = 0x1
	dmuOstZFS           = 2
	drrFlagClone        = 1 << 0
	backupFeatureRaw    = 1 << 24
	backupFeatureRedact = 1 << 25
)

// checkStreamHeader membaca rekaman pertama dari r dan memeriksanya.
// Mengembalikan byte yang sudah dibaca (diteruskan ke zfs receive).
func checkStreamHeader(r io.Reader, incremental bool) ([]byte, error) {
	hdr := make([]byte, drrBeginLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, errors.New("stream backup terlalu pendek")
	}
	var bo binary.ByteOrder = binary.LittleEndian
	if bo.Uint64(hdr[8:]) != dmuBackupMagic {
		bo = binary.BigEndian
		if bo.Uint64(hdr[8:]) != dmuBackupMagic {
			return nil, errors.New("bukan stream zfs send")
		}
	}
	vi := bo.Uint64(hdr[16:])
	switch {
	case bo.Uint32(hdr[0:]) != 0: // DRR_BEGIN
		return nil, errors.New("stream tidak diawali DRR_BEGIN")
	case vi&0x3 != dmuSubstream:
		return nil, errors.New("stream gabungan (zfs send -R/-p) tidak diterima")
	case (vi>>2)&(backupFeatureRaw|backupFeatureRedact) != 0:
		return nil, errors.New("stream raw/redacted tidak diterima")
	case bo.Uint32(hdr[32:]) != dmuOstZFS:
		return nil, errors.New("hanya stream filesystem yang diterima")
	case bo.Uint32(hdr[36:])&drrFlagClone != 0:
		return nil, errors.New("stream clone tidak diterima")
	case (bo.Uint64(hdr[48:]) != 0) != incremental:
		return nil, errors.New("jenis stream (penuh/inkremental) tidak sesuai permintaan")
	}
	return hdr, nil
}
