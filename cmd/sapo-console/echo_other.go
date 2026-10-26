//go:build !linux

package main

// setEcho: sapo-console hanya berjalan di NAS (Linux).
func setEcho(int, bool) {}
