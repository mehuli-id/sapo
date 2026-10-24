package agent

import (
	"encoding/binary"
	"testing"
)

func TestACLEntriesSortedAndEncoded(t *testing.T) {
	acl := shareACL{ReadUsers: []uint32{20012, 20011}, WriteUsers: []uint32{20011}, ReadGroups: []uint32{20013}}
	e := acl.entries(false)
	// Urutan tag lalu ID; tulis mengalahkan baca untuk akun yang sama.
	want := []aclEntry{
		{aclUserObj, aclRead | aclWrite, aclUndefinedID},
		{aclUser, aclRead | aclWrite, 20011},
		{aclUser, aclRead, 20012},
		{aclGroupObj, 0, aclUndefinedID},
		{aclGroup, aclRead, 20013},
		{aclMask, aclRead | aclWrite, aclUndefinedID},
		{aclOther, 0, aclUndefinedID},
	}
	if len(e) != len(want) {
		t.Fatalf("entri = %+v", e)
	}
	for i := range want {
		if e[i] != want[i] {
			t.Fatalf("entri %d = %+v, harus %+v", i, e[i], want[i])
		}
	}
	b := encodeACL(e)
	if binary.LittleEndian.Uint32(b) != 2 || len(b) != 4+8*len(e) || binary.LittleEndian.Uint32(b[4+8+4:]) != 20011 {
		t.Fatalf("encode = %x", b)
	}
	// Direktori: izin jalankan ikut.
	if d := acl.entries(true); d[0].Perm != aclRead|aclWrite|aclExec || d[2].Perm != aclRead|aclExec {
		t.Fatalf("direktori = %+v", d)
	}
}
