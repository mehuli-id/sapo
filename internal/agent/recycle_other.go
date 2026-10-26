//go:build !linux

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Di luar Linux (mode pengembangan) folder #recycle dibuat biasa dan isinya
// tidak dibersihkan.
func makeRecycleDir(root string) error {
	err := os.Mkdir(filepath.Join(root, recycleDir), 0o777)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

func cleanRecycle(string, time.Time, time.Time) (recycleStats, error) {
	return recycleStats{Done: true}, nil
}
