#!/usr/bin/env bash
# Membuat VM Debian untuk pengembangan Sapo dengan beberapa disk data virtual.
# Semua pengujian storage (pool, RAID, ganti disk) dilakukan di VM ini,
# TIDAK PERNAH di mesin yang berisi data penting.
#
# Kebutuhan di mesin pengembang (Linux): qemu-system-x86_64, qemu-img,
# cloud-localds (paket cloud-image-utils), curl, dan dukungan KVM.
#
# Pemakaian:
#   deploy/dev/create-vm.sh          # siapkan image + disk (sekali)
#   deploy/dev/create-vm.sh run      # jalankan VM
#   deploy/dev/create-vm.sh reset    # hapus semua disk data (mulai ulang uji storage)
set -euo pipefail

# --- Pengaturan (ubah lewat variabel lingkungan) ---------------------------
VM_DIR="${VM_DIR:-$(dirname "$0")/.vm}"
DEBIAN_IMAGE_URL="${DEBIAN_IMAGE_URL:-https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2}"
VM_RAM_MB="${VM_RAM_MB:-4096}"
VM_CPUS="${VM_CPUS:-2}"
DATA_DISKS="${DATA_DISKS:-6}"          # jumlah disk data virtual
DATA_DISK_SIZE="${DATA_DISK_SIZE:-8G}" # ukuran tiap disk data
SSH_PORT="${SSH_PORT:-2222}"           # ssh -p 2222 sapo@localhost
PANEL_PORT="${PANEL_PORT:-5000}"       # http://localhost:5000
SSH_PUBKEY="${SSH_PUBKEY:-$HOME/.ssh/id_ed25519.pub}"
# ---------------------------------------------------------------------------

mkdir -p "$VM_DIR"
cmd="${1:-setup}"

setup() {
  if [[ ! -f "$VM_DIR/base.qcow2" ]]; then
    echo "Mengunduh image Debian cloud…"
    curl -fL --output "$VM_DIR/base.qcow2" "$DEBIAN_IMAGE_URL"
  fi
  if [[ ! -f "$VM_DIR/system.qcow2" ]]; then
    qemu-img create -f qcow2 -F qcow2 -b base.qcow2 "$VM_DIR/system.qcow2" 32G
  fi
  for i in $(seq 1 "$DATA_DISKS"); do
    [[ -f "$VM_DIR/data$i.qcow2" ]] || qemu-img create -f qcow2 "$VM_DIR/data$i.qcow2" "$DATA_DISK_SIZE"
  done
  if [[ ! -f "$SSH_PUBKEY" ]]; then
    echo "Kunci SSH publik tidak ditemukan di $SSH_PUBKEY (atur SSH_PUBKEY)." >&2
    exit 1
  fi
  cat > "$VM_DIR/user-data" <<EOF
#cloud-config
hostname: sapo-dev
users:
  - name: sapo
    groups: [sudo]
    shell: /bin/bash
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    ssh_authorized_keys:
      - $(cat "$SSH_PUBKEY")
package_update: true
packages: [smartmontools, avahi-daemon, rsync]
EOF
  cloud-localds "$VM_DIR/seed.iso" "$VM_DIR/user-data"
  echo "Siap. Jalankan: $0 run"
}

run() {
  local disks=()
  for i in $(seq 1 "$DATA_DISKS"); do
    disks+=(-drive "file=$VM_DIR/data$i.qcow2,if=none,id=d$i,format=qcow2"
            -device "virtio-blk-pci,drive=d$i,serial=SAPO-DATA-$i")
  done
  exec qemu-system-x86_64 \
    -enable-kvm -cpu host -smp "$VM_CPUS" -m "$VM_RAM_MB" \
    -drive "file=$VM_DIR/system.qcow2,if=virtio,format=qcow2" \
    -drive "file=$VM_DIR/seed.iso,if=virtio,format=raw,readonly=on" \
    "${disks[@]}" \
    -nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22,hostfwd=tcp:127.0.0.1:$PANEL_PORT-:5000" \
    -nographic
}

reset_disks() {
  read -r -p "Hapus dan buat ulang $DATA_DISKS disk data VM? [ketik ya] " ok
  [[ "$ok" == "ya" ]] || exit 1
  for i in $(seq 1 "$DATA_DISKS"); do
    rm -f "$VM_DIR/data$i.qcow2"
    qemu-img create -f qcow2 "$VM_DIR/data$i.qcow2" "$DATA_DISK_SIZE"
  done
}

case "$cmd" in
  setup) setup ;;
  run) run ;;
  reset) reset_disks ;;
  *) echo "Perintah tidak dikenal: $cmd (setup | run | reset)" >&2; exit 1 ;;
esac
