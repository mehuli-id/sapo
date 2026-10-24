//go:build !linux

package agent

// openExclusive: di luar Linux (mode pengembangan) tidak ada perangkat blok
// yang bisa diperiksa; pool memang tidak dibuat di sana.
func openExclusive(string) error { return nil }
