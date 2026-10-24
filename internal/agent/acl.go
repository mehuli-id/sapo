package agent

import (
	"encoding/binary"
	"sort"
)

// ACL POSIX dalam format xattr Linux (system.posix_acl_access dan
// system.posix_acl_default): header versi 2, lalu entri {tag, izin, id}.
// Ditulis langsung (bukan lewat setfacl) agar penerapan ke isi folder yang
// besar cepat dan tidak pernah mengikuti symlink (lihat acl_linux.go).

const (
	aclXattrVersion = 2
	aclUserObj      = 0x01
	aclUser         = 0x02
	aclGroupObj     = 0x04
	aclGroup        = 0x08
	aclMask         = 0x10
	aclOther        = 0x20
	aclUndefinedID  = 0xffffffff

	aclRead  = 4
	aclWrite = 2
	aclExec  = 1
)

type aclEntry struct {
	Tag  uint16
	Perm uint16
	ID   uint32
}

// shareACL adalah izin satu folder dalam bentuk ID numerik.
type shareACL struct {
	ReadUsers, WriteUsers   []uint32
	ReadGroups, WriteGroups []uint32
}

// entries menyusun ACL untuk direktori (exec = true) atau file. Pemilik
// tetap punya akses penuh; grup pemilik dan lainnya tanpa akses.
func (a shareACL) entries(exec bool) []aclEntry {
	x := uint16(0)
	if exec {
		x = aclExec
	}
	rw := uint16(aclRead|aclWrite) | x
	ro := uint16(aclRead) | x
	out := []aclEntry{
		{aclUserObj, rw, aclUndefinedID},
		{aclGroupObj, 0, aclUndefinedID},
		{aclMask, rw, aclUndefinedID},
		{aclOther, 0, aclUndefinedID},
	}
	perm := map[[2]uint32]uint16{}
	add := func(tag uint16, ids []uint32, p uint16) {
		for _, id := range ids {
			k := [2]uint32{uint32(tag), id}
			perm[k] |= p
		}
	}
	add(aclUser, a.ReadUsers, ro)
	add(aclUser, a.WriteUsers, rw)
	add(aclGroup, a.ReadGroups, ro)
	add(aclGroup, a.WriteGroups, rw)
	for k, p := range perm {
		out = append(out, aclEntry{uint16(k[0]), p, k[1]})
	}
	// Kernel mewajibkan urutan tag lalu ID.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tag != out[j].Tag {
			return out[i].Tag < out[j].Tag
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func encodeACL(entries []aclEntry) []byte {
	b := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(b, aclXattrVersion)
	for i, e := range entries {
		o := 4 + 8*i
		binary.LittleEndian.PutUint16(b[o:], e.Tag)
		binary.LittleEndian.PutUint16(b[o+2:], e.Perm)
		binary.LittleEndian.PutUint32(b[o+4:], e.ID)
	}
	return b
}
