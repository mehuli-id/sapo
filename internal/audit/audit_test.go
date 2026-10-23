package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotationCapsSize(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, 2048)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 500; i++ {
		if err := l.Record(Event{Action: "auth.login", Actor: "penyerang"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"audit.log", "audit.log.1"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > 2048 {
			t.Fatalf("%s %d byte, melewati batas", name, st.Size())
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s izin %v", name, st.Mode().Perm())
		}
	}
}
