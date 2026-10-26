package main

import "golang.org/x/sys/unix"

// setEcho menyalakan/mematikan gema terminal (password Wi-Fi tidak tampil
// di layar). Bukan terminal: tidak ada yang diubah.
func setEcho(fd int, on bool) {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return
	}
	if on {
		t.Lflag |= unix.ECHO
	} else {
		t.Lflag &^= unix.ECHO
	}
	_ = unix.IoctlSetTermios(fd, unix.TCSETS, t)
}
