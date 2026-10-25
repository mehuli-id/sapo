package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

// Installer ke disk (ADR 0016): saat berjalan dari ISO (RAM), Sapo dipasang
// ke satu disk sistem dengan menyalin sistem dari squashfs ISO (persis yang
// diuji, tanpa internet), lalu pengaturan sesi live (akun, pengaturan panel,
// jaringan, pool) disalin ke sana. Pemasangan berjalan di unit systemd
// tersendiri (sapod install-run), terpisah dari sapod.

// InstallTools mengatur installer (agent.install di sapo.yaml).
type InstallTools struct {
	// LiveMarker: ada = berjalan dari ISO. SquashfsRoot: sistem dari ISO
	// (lapisan bawah overlay live, tanpa perubahan sesi).
	LiveMarker, SquashfsRoot string
	MinDiskBytes             uint64
	SystemdRun, Systemctl    string
	Self, Unit               string
	ParamsFile, LogFile      string
	ResultFile               string
	Timeout                  time.Duration
	Target                   string
	Programs                 InstallPrograms
	// StopService: dihentikan selama pengaturan disalin (sapo-api).
	StopService            string
	KernelCmdline          string
	CopyPaths, RemovePaths []string
	RemoveUsers            []string
	PurgePackages          []string
}

// InstallPrograms: program yang dipakai install-run (path absolut).
type InstallPrograms struct {
	Wipefs, Sgdisk, MkfsVfat, MkfsExt4 string
	Mount, Umount, Cp, Chroot, Blkid   string
	Udevadm, Sync, Lsblk               string
}

// Install melayani install.*.
type Install struct {
	t     InstallTools
	st    *Storage
	pools *Pools
	mu    sync.Mutex
}

// NewInstall menyiapkan installer.
func NewInstall(t InstallTools, st *Storage, pools *Pools) *Install {
	in := &Install{t: t, st: st, pools: pools}
	pools.installing = in.running
	return in
}

func (in *Install) live() bool {
	_, err := os.Stat(in.t.LiveMarker)
	return err == nil
}

func (in *Install) running(ctx context.Context) bool {
	res, err := runTool(ctx, 10*time.Second, in.t.Systemctl, []string{"is-active"}, in.t.Unit)
	return err == nil && strings.TrimSpace(string(res.Stdout)) == "active"
}

// StatusOp mengembalikan handler install.status.
func (in *Install) StatusOp() Handler {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		out := proto.InstallStatus{Live: in.live(), Running: in.running(ctx), Log: []string{}, MinDiskBytes: in.t.MinDiskBytes}
		if data, err := os.ReadFile(in.t.ResultFile); err == nil {
			var r proto.InstallRun
			if json.Unmarshal(data, &r) == nil {
				r.Interrupted = !out.Running && r.FinishedAt.IsZero()
				out.Last = &r
			}
		}
		if f, err := os.Open(in.t.LogFile); err == nil {
			defer f.Close()
			if st, err := f.Stat(); err == nil && st.Size() > 32<<10 {
				_, _ = f.Seek(st.Size()-32<<10, io.SeekStart)
			}
			b, _ := io.ReadAll(io.LimitReader(f, 32<<10))
			lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			if len(lines) > 30 {
				lines = lines[len(lines)-30:]
			}
			for _, l := range lines {
				out.Log = append(out.Log, printableLine(l))
			}
		}
		return out, nil
	}
}

// installRunParams: isi file parameter install-run (ditulis sapod, 0600).
// Ref: identitas disk yang dipilih; install-run memeriksanya lagi tepat
// sebelum menghapus disk.
type installRunParams struct {
	Disk     string        `json:"disk"`
	Tools    InstallTools  `json:"tools"`
	Started  time.Time     `json:"started"`
	DiskName string        `json:"disk_name"`
	Ref      proto.DiskRef `json:"ref"`
}

// StartOp mengembalikan handler install.start. Disk diperiksa seketat disk
// pool baru (identitas sama dengan yang dipilih, tidak dipakai, bukan USB
// atau disk lepas-pasang, penghapusan dikonfirmasi), dan harus cukup besar.
func (in *Install) StartOp() Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p proto.InstallStartParams
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
		if !deviceNamePattern.MatchString(p.Disk.Name) {
			return nil, &OpError{Code: proto.ErrInvalidParams, Message: "nama disk tidak valid"}
		}
		if !in.live() {
			return nil, &OpError{Code: "install_not_live", Message: "installer hanya tersedia saat Sapo berjalan dari ISO"}
		}
		if fi, err := os.Stat(in.t.SquashfsRoot); err != nil || !fi.IsDir() {
			return nil, &OpError{Code: "install_not_live", Message: "sistem ISO (squashfs) tidak ditemukan"}
		}
		if !in.mu.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "pemasangan sedang dimulai"}
		}
		defer in.mu.Unlock()
		if in.running(ctx) {
			return nil, &OpError{Code: proto.ErrBusy, Message: "pemasangan sedang berjalan"}
		}
		if isMountPoint(in.t.Target) {
			return nil, &OpError{Code: proto.ErrConflict, Message: "pemasangan sebelumnya masih terpasang di " + in.t.Target + "; nyalakan ulang dulu"}
		}
		// Disk media ISO tidak pernah dipilih (walau tidak terlihat terpasang,
		// misalnya boot dengan toram).
		if dev := mountSource(in.t.LiveMarker); dev != "" && (dev == p.Disk.Name || strings.HasPrefix(dev, p.Disk.Name)) {
			return nil, &OpError{Code: proto.ErrDiskInUse, Message: "disk " + p.Disk.Name + " adalah media ISO yang sedang dipakai"}
		}
		// Tidak bersamaan dengan operasi pool (disk yang sama bisa dipilih).
		if !in.pools.write.TryLock() {
			return nil, &OpError{Code: proto.ErrBusy, Message: "operasi pool lain sedang berjalan"}
		}
		defer in.pools.write.Unlock()
		in.st.forgetDisks()
		disks, err := in.st.listDisks(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := in.pools.checkDisks([]proto.DiskRef{p.Disk}, disks, false); err != nil {
			return nil, err
		}
		var disk proto.Disk
		for _, d := range disks {
			if d.Name == p.Disk.Name {
				disk = d
			}
		}
		if disk.SizeBytes < in.t.MinDiskBytes {
			return nil, &OpError{Code: "install_disk_small", Message: fmt.Sprintf("disk terlalu kecil (minimal %d GB)", in.t.MinDiskBytes/1e9)}
		}
		params := installRunParams{Disk: "/dev/" + disk.Name, DiskName: disk.Name, Tools: in.t, Started: time.Now().UTC(), Ref: p.Disk}
		data, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		if err := writePrivateFile(in.t.ParamsFile, data); err != nil {
			return nil, err
		}
		_ = os.Remove(in.t.ResultFile)
		res, err := runTool(ctx, 30*time.Second, in.t.SystemdRun, []string{
			"--unit=" + in.t.Unit, "--description=Pemasangan Sapo ke disk", "--collect", "--quiet",
			"--property=Type=exec", fmt.Sprintf("--property=RuntimeMaxSec=%d", int(in.t.Timeout.Seconds())),
		}, in.t.Self, "install-run", in.t.ParamsFile)
		if err != nil {
			_ = os.Remove(in.t.ParamsFile)
			return nil, err
		}
		if res.ExitCode != 0 {
			_ = os.Remove(in.t.ParamsFile)
			return nil, &OpError{Code: "install_failed", Message: clipTo(firstLine(res.Stderr), maxErrorText)}
		}
		in.pools.logInfo("pemasangan ke disk dimulai", "disk", disk.Name)
		return map[string]bool{"ok": true}, nil
	}
}

// mountSource: nama perangkat (tanpa /dev/) yang terpasang di mountpoint,
// dari /proc/self/mounts; kosong bila tidak ada.
func mountSource(mountpoint string) string {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == mountpoint && strings.HasPrefix(f[0], "/dev/") {
			return strings.TrimPrefix(f[0], "/dev/")
		}
	}
	return ""
}

// partPath: nama partisi n disk (sda → sda3, nvme0n1 → nvme0n1p3).
func partPath(disk string, n int) string {
	if last := disk[len(disk)-1]; last >= '0' && last <= '9' {
		return fmt.Sprintf("%sp%d", disk, n)
	}
	return fmt.Sprintf("%s%d", disk, n)
}

// installer menjalankan langkah pemasangan; run dan file sistem diganti di
// test.
type installer struct {
	p   installRunParams
	log io.Writer
	// run menjalankan program dan mengembalikan keluaran standarnya.
	run func(prog string, args ...string) (string, error)
	// root: akar sistem live (untuk menyalin pengaturan; "/" di sistem
	// sebenarnya, direktori sementara di test).
	root string
	step func(name string)
	// wait menunggu partisi baru muncul (diganti di test).
	wait func(path string, d time.Duration) error
	// verify memeriksa ulang identitas disk tepat sebelum dihapus (diganti
	// di test).
	verify func() error
}

func (r *installer) do(prog string, args ...string) error {
	_, err := r.run(prog, args...)
	return err
}

// chroot menjalankan perintah di sistem baru.
func (r *installer) chroot(args ...string) error {
	return r.do(r.p.Tools.Programs.Chroot, append([]string{"--", r.p.Tools.Target}, args...)...)
}

func (r *installer) target(p string) string { return filepath.Join(r.p.Tools.Target, p) }

// install memasang ke disk. Langkah-langkahnya dijalankan berurutan; bila
// satu gagal, pemasangan berhenti (disk sistem belum bisa dipakai, data di
// pool tidak tersentuh).
func (r *installer) install() error {
	t, pr := r.p.Tools, r.p.Tools.Programs
	disk := r.p.Disk
	bios, esp, rootPart := partPath(disk, 1), partPath(disk, 2), partPath(disk, 3)
	var mounts []string
	defer func() {
		// Dilepas terbalik, apa pun hasilnya; bila gagal, lazy.
		for i := len(mounts) - 1; i >= 0; i-- {
			if err := r.do(pr.Umount, "--", mounts[i]); err != nil {
				_ = r.do(pr.Umount, "-l", "--", mounts[i])
			}
		}
	}()
	if isMountPoint(t.Target) {
		return fmt.Errorf("%s masih terpasang dari pemasangan sebelumnya", t.Target)
	}
	r.step("periksa disk")
	if err := r.verify(); err != nil {
		return err
	}
	mount := func(args ...string) error {
		if err := r.do(pr.Mount, args...); err != nil {
			return err
		}
		mounts = append(mounts, args[len(args)-1])
		return nil
	}

	r.step("partisi")
	if err := r.do(pr.Wipefs, "--all", "--", disk); err != nil {
		return err
	}
	if err := r.do(pr.Sgdisk, "--zap-all", "--", disk); err != nil {
		return err
	}
	// BIOS boot (GRUB untuk BIOS lama), EFI, dan root.
	if err := r.do(pr.Sgdisk,
		"--new=1:0:+1M", "--typecode=1:EF02", "--change-name=1:sapo-bios",
		"--new=2:0:+512M", "--typecode=2:EF00", "--change-name=2:sapo-efi",
		"--new=3:0:0", "--typecode=3:8300", "--change-name=3:sapo-root",
		"--", disk); err != nil {
		return err
	}
	_ = r.do(pr.Udevadm, "settle")
	for _, part := range []string{bios, esp, rootPart} {
		if err := r.wait(part, 30*time.Second); err != nil {
			return err
		}
	}

	r.step("format")
	if err := r.do(pr.MkfsVfat, "-F", "32", "-n", "SAPO-EFI", "--", esp); err != nil {
		return err
	}
	if err := r.do(pr.MkfsExt4, "-q", "-F", "-L", "sapo-root", "--", rootPart); err != nil {
		return err
	}

	r.step("salin sistem")
	if err := os.MkdirAll(t.Target, 0o755); err != nil {
		return err
	}
	if err := mount("--", rootPart, t.Target); err != nil {
		return err
	}
	if err := r.do(pr.Cp, "-a", "--one-file-system", "--", t.SquashfsRoot+"/.", t.Target+"/"); err != nil {
		return err
	}
	if err := os.MkdirAll(r.target("boot/efi"), 0o755); err != nil {
		return err
	}
	if err := mount("--", esp, r.target("boot/efi")); err != nil {
		return err
	}

	r.step("salin pengaturan")
	// sapo-api dihentikan selama menyalin agar datanya utuh (panel tidak
	// bisa dibuka sebentar).
	if t.StopService != "" {
		_ = r.do(t.Systemctl, "stop", "--", t.StopService)
		defer func() { _ = r.do(t.Systemctl, "start", "--", t.StopService) }()
	}
	for _, p := range t.CopyPaths {
		src := filepath.Join(r.root, p)
		dst, err := r.safeTarget(p)
		if err != nil {
			return err
		}
		// Disamakan dengan sesi live: yang tidak ada di sana juga dibuang.
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if _, err := os.Lstat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := r.do(pr.Cp, "-a", "--", src, dst); err != nil {
			return err
		}
	}
	// Berkas khusus ISO (tmpfs untuk image, log ke serial, …) dibuang.
	for _, p := range t.RemovePaths {
		dst, err := r.safeTarget(p)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}

	r.step("fstab")
	rootUUID, err := r.run(pr.Blkid, "-s", "UUID", "-o", "value", "--", rootPart)
	if err != nil {
		return err
	}
	espUUID, err := r.run(pr.Blkid, "-s", "UUID", "-o", "value", "--", esp)
	if err != nil {
		return err
	}
	rootUUID, espUUID = strings.TrimSpace(rootUUID), strings.TrimSpace(espUUID)
	if !uuidPattern.MatchString(rootUUID) || !uuidPattern.MatchString(espUUID) {
		return errors.New("UUID partisi baru tidak terbaca")
	}
	fstab := fmt.Sprintf("# Dibuat installer Sapo.\nUUID=%s / ext4 errors=remount-ro 0 1\nUUID=%s /boot/efi vfat umask=0077 0 2\n", rootUUID, espUUID)
	if err := os.WriteFile(r.target("etc/fstab"), []byte(fstab), 0o644); err != nil {
		return err
	}
	// machine-id baru untuk sistem terpasang.
	_ = os.Remove(r.target("etc/machine-id"))

	r.step("bootloader")
	for _, m := range [][]string{
		{"--bind", "--", "/dev", r.target("dev")},
		{"-t", "proc", "--", "proc", r.target("proc")},
		{"-t", "sysfs", "--", "sysfs", r.target("sys")},
	} {
		if err := mount(m...); err != nil {
			return err
		}
	}
	if fi, err := os.Stat("/sys/firmware/efi/efivars"); err == nil && fi.IsDir() {
		_ = mount("-t", "efivarfs", "--", "efivarfs", r.target("sys/firmware/efi/efivars"))
	}
	if err := r.chroot("systemd-machine-id-setup"); err != nil {
		return err
	}
	// Akun live (password umum, sudo) tidak boleh terbawa: gagal-tertutup.
	for _, u := range t.RemoveUsers {
		if hasAccount(r.target("etc/passwd"), u) {
			// Direktori rumah dari sesi live tidak tersalin, jadi userdel
			// boleh mengeluh soal itu; yang diperiksa hasilnya di bawah.
			_ = r.chroot("userdel", "--remove", "--", u)
		}
		for _, f := range []string{"etc/passwd", "etc/shadow", "etc/group", "etc/gshadow"} {
			if hasAccount(r.target(f), u) || hasMember(r.target(f), u) {
				return fmt.Errorf("akun live %s masih ada di %s", u, f)
			}
		}
	}
	if len(t.PurgePackages) > 0 {
		if err := r.chroot(append([]string{"dpkg", "--purge", "--force-depends", "--"}, t.PurgePackages...)...); err != nil {
			return err
		}
	}
	grubDefault := fmt.Sprintf("# Dibuat installer Sapo.\nGRUB_DEFAULT=0\nGRUB_TIMEOUT=3\nGRUB_DISTRIBUTOR=Sapo\nGRUB_CMDLINE_LINUX_DEFAULT=%q\nGRUB_CMDLINE_LINUX=\"\"\n", t.KernelCmdline)
	if err := os.WriteFile(r.target("etc/default/grub"), []byte(grubDefault), 0o644); err != nil {
		return err
	}
	// Kernel baru (pembaruan sistem) ikut masuk menu GRUB.
	hook := "#!/bin/sh\n# Dibuat installer Sapo: menu GRUB diperbarui setelah kernel dipasang/dihapus.\nexec grub-mkconfig -o /boot/grub/grub.cfg >/dev/null 2>&1\n"
	for _, d := range []string{"etc/kernel/postinst.d", "etc/kernel/postrm.d"} {
		if err := os.MkdirAll(r.target(d), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(r.target(filepath.Join(d, "zz-sapo-grub")), []byte(hook), 0o755); err != nil {
			return err
		}
	}
	if err := r.chroot("update-initramfs", "-u", "-k", "all"); err != nil {
		return err
	}
	if err := r.chroot("grub-install", "--target=i386-pc", "--", disk); err != nil {
		return err
	}
	if err := r.chroot("grub-install", "--target=x86_64-efi", "--efi-directory=/boot/efi", "--removable", "--no-nvram"); err != nil {
		return err
	}
	if err := r.chroot("grub-mkconfig", "-o", "/boot/grub/grub.cfg"); err != nil {
		return err
	}
	r.step("selesai")
	_ = r.do(pr.Sync)
	return nil
}

func waitPath(p string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(p); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("partisi %s tidak muncul", p)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// InstallRunMain adalah "sapod install-run <parameter>": dijalankan
// systemd-run sebagai root. Mencatat setiap langkah ke log dan hasil ke file
// hasil.
func InstallRunMain(args []string) int {
	if len(args) != 1 || !filepath.IsAbs(args[0]) {
		fmt.Fprintln(os.Stderr, "pemakaian: sapod install-run <parameter.json>")
		return 2
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Sekali pakai: file parameter dibuang begitu dibaca.
	_ = os.Remove(args[0])
	var p installRunParams
	if err := json.Unmarshal(data, &p); err != nil || !strings.HasPrefix(p.Disk, "/dev/") || !deviceNamePattern.MatchString(p.DiskName) || p.Disk != "/dev/"+p.DiskName || p.Ref.Name != p.DiskName {
		fmt.Fprintln(os.Stderr, "parameter pemasangan tidak valid")
		return 2
	}
	if _, err := os.Stat(p.Tools.LiveMarker); err != nil {
		fmt.Fprintln(os.Stderr, "installer hanya berjalan dari ISO")
		return 2
	}
	// SIGTERM (batas waktu unit, systemctl stop): perintah yang berjalan
	// dihentikan dan titik pasang tetap dilepas.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	run := proto.InstallRun{Disk: p.DiskName, StartedAt: time.Now().UTC()}
	writeResult := func() {
		b, _ := json.Marshal(run)
		_ = writePrivateFile(p.Tools.ResultFile, b)
	}
	logf, err := os.OpenFile(p.Tools.LogFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		run.Error, run.FinishedAt = err.Error(), time.Now().UTC()
		writeResult()
		return 1
	}
	defer logf.Close()
	w := bufio.NewWriter(logf)
	flush := func() { _ = w.Flush() }
	r := &installer{p: p, log: w, root: "/", wait: waitPath,
		verify: func() error { return verifyDisk(p.Tools.Programs.Lsblk, p.Ref, p.DiskName) },
		run: func(prog string, args ...string) (string, error) {
			fmt.Fprintf(w, "$ %s %s\n", filepath.Base(prog), strings.Join(args, " "))
			flush()
			// Melepas titik pasang tetap dijalankan walau sudah dihentikan.
			runCtx := ctx
			if prog == p.Tools.Programs.Umount {
				runCtx = context.Background()
			}
			cmd := exec.CommandContext(runCtx, prog, args...)
			cmd.Dir = "/"
			cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "DEBIAN_FRONTEND=noninteractive"}
			var out strings.Builder
			cmd.Stdout = io.MultiWriter(&out, w)
			cmd.Stderr = w
			err := cmd.Run()
			flush()
			if err != nil {
				return out.String(), fmt.Errorf("%s: %w", filepath.Base(prog), err)
			}
			return out.String(), nil
		},
	}
	r.step = func(name string) {
		run.Step = name
		fmt.Fprintf(w, "== %s\n", name)
		flush()
		writeResult()
	}
	writeResult()
	err = r.install()
	run.FinishedAt = time.Now().UTC()
	run.OK = err == nil
	if err != nil {
		run.Error = clipTo(err.Error(), maxErrorText)
		fmt.Fprintf(w, "gagal: %v\n", err)
	} else {
		fmt.Fprintln(w, "selesai: Sapo terpasang; cabut media ISO lalu nyalakan ulang")
	}
	flush()
	writeResult()
	if err != nil {
		return 1
	}
	return 0
}

// safeTarget: path p di dalam Target tanpa melewati symlink yang mengarah
// keluar Target (misalnya /var/run → /run milik sistem live).
func (r *installer) safeTarget(p string) (string, error) {
	dst := r.target(p)
	dir := filepath.Dir(dst)
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		dir = filepath.Dir(dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	base, err := filepath.EvalSymlinks(r.p.Tools.Target)
	if err != nil {
		return "", err
	}
	if resolved != base && !strings.HasPrefix(resolved+"/", base+"/") {
		return "", fmt.Errorf("%s melewati symlink keluar dari sistem baru", p)
	}
	return dst, nil
}

// hasAccount: baris "<nama>:" ada di file akun (passwd, shadow, group,
// gshadow).
func hasAccount(path, name string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, name+":") {
			return true
		}
	}
	return false
}

// hasMember: name tercatat sebagai anggota grup (kolom terakhir group atau
// gshadow).
func hasMember(path, name string) bool {
	if !strings.HasSuffix(path, "group") && !strings.HasSuffix(path, "gshadow") {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 4 && slices.Contains(strings.Split(f[len(f)-1], ","), name) {
			return true
		}
	}
	return false
}

// verifyDisk: disk di path masih disk yang dipilih (nomor seri, WWN,
// ukuran) dan tidak sedang dipakai (lsblk dibaca ulang).
func verifyDisk(lsblk string, ref proto.DiskRef, name string) error {
	out, err := exec.Command(lsblk, lsblkFlags...).Output()
	if err != nil {
		return fmt.Errorf("lsblk: %w", err)
	}
	disks, err := parseLsblk(out, nil)
	if err != nil {
		return err
	}
	for _, d := range disks {
		if d.Name != name {
			continue
		}
		if d.Serial != ref.Serial || d.WWN != ref.WWN || d.SizeBytes != ref.SizeBytes {
			return fmt.Errorf("disk %s sudah berbeda dari yang dipilih; tidak ada yang dihapus", name)
		}
		if d.Usage == proto.DiskInUse {
			return fmt.Errorf("disk %s sedang dipakai (%s); tidak ada yang dihapus", name, d.UsageReason)
		}
		return nil
	}
	return fmt.Errorf("disk %s tidak ditemukan; tidak ada yang dihapus", name)
}
