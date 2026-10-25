package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mehuli-id/sapo/internal/proto"
)

func TestPartPath(t *testing.T) {
	for disk, want := range map[string]string{"/dev/sda": "/dev/sda3", "/dev/vdb": "/dev/vdb3", "/dev/nvme0n1": "/dev/nvme0n1p3", "/dev/mmcblk0": "/dev/mmcblk0p3"} {
		if got := partPath(disk, 3); got != want {
			t.Errorf("%s → %s, mau %s", disk, got, want)
		}
	}
}

// Urutan pemasangan dengan program tiruan: partisi, format, salin sistem,
// salin pengaturan, buang berkas ISO, fstab, bootloader; semua mount
// dilepas lagi.
func TestInstallerSteps(t *testing.T) {
	base := t.TempDir()
	live, target := filepath.Join(base, "live"), filepath.Join(base, "target")
	for _, d := range []string{"live/etc/sapo", "target/etc/systemd/system", "target/etc/default", "target/etc/issue.d"} {
		_ = os.MkdirAll(filepath.Join(base, d), 0o755)
	}
	_ = os.WriteFile(filepath.Join(live, "etc/sapo/sapo.yaml"), []byte("api: {}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(target, "etc/issue.d/sapo-iso.issue"), []byte("ISO"), 0o644)
	var calls []string
	r := &installer{
		root: live,
		p: installRunParams{Disk: "/dev/vdd", DiskName: "vdd", Tools: InstallTools{
			SquashfsRoot: "/run/live/rootfs/filesystem.squashfs", Target: target, KernelCmdline: "quiet",
			CopyPaths: []string{"/etc/sapo", "/etc/tidak-ada"}, RemovePaths: []string{"/etc/issue.d/sapo-iso.issue"},
			RemoveUsers: []string{"user"}, PurgePackages: []string{"live-boot"},
			Programs: InstallPrograms{Wipefs: "wipefs", Sgdisk: "sgdisk", MkfsVfat: "mkfs.vfat", MkfsExt4: "mkfs.ext4", Mount: "mount",
				Umount: "umount", Cp: "cp", Chroot: "chroot", Blkid: "blkid", Udevadm: "udevadm", Sync: "sync"},
		}},
		run: func(prog string, args ...string) (string, error) {
			calls = append(calls, prog+" "+strings.Join(args, " "))
			switch {
			case prog == "blkid":
				return "1111-2222\n", nil
			case prog == "cp" && strings.HasSuffix(args[len(args)-1], "/etc/sapo"):
				// Salinan pengaturan: dibuat sungguhan agar bisa diperiksa.
				_ = os.MkdirAll(args[len(args)-1], 0o755)
			}
			return "", nil
		},
		step: func(string) {},
		wait: func(string, time.Duration) error { return nil },
	}
	if err := r.install(); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(calls, "\n") + "\n"
	inOrder(t, all,
		"wipefs --all -- /dev/vdd",
		"sgdisk --zap-all -- /dev/vdd",
		"sgdisk --new=1:0:+1M --typecode=1:EF02 --change-name=1:sapo-bios --new=2:0:+512M --typecode=2:EF00 --change-name=2:sapo-efi --new=3:0:0 --typecode=3:8300 --change-name=3:sapo-root -- /dev/vdd",
		"mkfs.vfat -F 32 -n SAPO-EFI -- /dev/vdd2",
		"mkfs.ext4 -q -F -L sapo-root -- /dev/vdd3",
		"mount -- /dev/vdd3 "+target,
		"cp -a --one-file-system -- /run/live/rootfs/filesystem.squashfs/. "+target+"/",
		"mount -- /dev/vdd2 "+target+"/boot/efi",
		"cp -a -- "+live+"/etc/sapo "+target+"/etc/sapo",
		"chroot -- "+target+" userdel --remove -- user",
		"chroot -- "+target+" dpkg --purge --force-depends -- live-boot",
		"chroot -- "+target+" update-initramfs -u -k all",
		"chroot -- "+target+" grub-install --target=i386-pc -- /dev/vdd",
		"chroot -- "+target+" grub-install --target=x86_64-efi --efi-directory=/boot/efi --removable --no-nvram",
		"chroot -- "+target+" grub-mkconfig -o /boot/grub/grub.cfg",
		"umount -- "+target+"/boot/efi",
		"umount -- "+target,
	)
	if strings.Contains(all, "tidak-ada") {
		t.Fatal("path yang tidak ada tidak disalin")
	}
	if _, err := os.Stat(filepath.Join(target, "etc/issue.d/sapo-iso.issue")); err == nil {
		t.Fatal("berkas khusus ISO harus dibuang")
	}
	fstab, _ := os.ReadFile(filepath.Join(target, "etc/fstab"))
	if !strings.Contains(string(fstab), "UUID=1111-2222 / ext4") || !strings.Contains(string(fstab), "UUID=1111-2222 /boot/efi vfat") {
		t.Fatalf("fstab:\n%s", fstab)
	}
	if g, _ := os.ReadFile(filepath.Join(target, "etc/default/grub")); !strings.Contains(string(g), `GRUB_CMDLINE_LINUX_DEFAULT="quiet"`) {
		t.Fatalf("grub:\n%s", g)
	}
	if _, err := os.Stat(filepath.Join(target, "etc/kernel/postinst.d/zz-sapo-grub")); err != nil {
		t.Fatal("hook GRUB untuk kernel baru tidak ada")
	}
}

// Langkah gagal: pemasangan berhenti, mount yang sudah dibuat dilepas.
func TestInstallerStopsOnFailure(t *testing.T) {
	target := t.TempDir()
	var calls []string
	r := &installer{
		root: t.TempDir(),
		p:    installRunParams{Disk: "/dev/vdd", Tools: InstallTools{Target: target, Programs: InstallPrograms{Wipefs: "wipefs", Sgdisk: "sgdisk", MkfsVfat: "mkfs.vfat", MkfsExt4: "mkfs.ext4", Mount: "mount", Umount: "umount", Cp: "cp"}}},
		run: func(prog string, args ...string) (string, error) {
			calls = append(calls, prog+" "+strings.Join(args, " "))
			if prog == "cp" {
				return "", errors.New("disk penuh")
			}
			return "", nil
		},
		step: func(string) {},
		wait: func(string, time.Duration) error { return nil },
	}
	if err := r.install(); err == nil {
		t.Fatal("galat harus dilaporkan")
	}
	all := strings.Join(calls, "\n")
	if !strings.HasSuffix(all, "umount -- "+target) || strings.Contains(all, "grub-install") {
		t.Fatalf("panggilan:\n%s", all)
	}
}

func TestInstallStartRefusesOutsideLive(t *testing.T) {
	p, _ := newTestPools(t, zpoolFake{names: "tank\n"})
	in := NewInstall(InstallTools{LiveMarker: filepath.Join(t.TempDir(), "tidak-ada")}, p.st, p)
	raw, _ := json.Marshal(proto.InstallStartParams{Disk: proto.DiskRef{Name: "vdd"}})
	var oe *OpError
	if _, err := in.StartOp()(context.Background(), raw); !errors.As(err, &oe) || oe.Code != "install_not_live" {
		t.Fatalf("di luar ISO: %v", err)
	}
	raw, _ = json.Marshal(proto.InstallStartParams{Disk: proto.DiskRef{Name: "../sda"}})
	if _, err := in.StartOp()(context.Background(), raw); !errors.As(err, &oe) || oe.Code != proto.ErrInvalidParams {
		t.Fatalf("nama disk aneh: %v", err)
	}
}
