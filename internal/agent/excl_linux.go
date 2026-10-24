package agent

import (
	"os"
	"syscall"
)

// openExclusive membuka perangkat blok dengan O_EXCL tanpa O_CREAT: di Linux
// ini gagal EBUSY bila perangkat sedang dipegang pihak lain (sistem berkas
// terpasang, swap, RAID, device-mapper, btrfs). Tidak ada yang ditulis.
func openExclusive(dev string) error {
	f, err := os.OpenFile(dev, os.O_RDONLY|syscall.O_EXCL, 0)
	if err != nil {
		return err
	}
	return f.Close()
}
