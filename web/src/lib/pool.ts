// Arti susunan pool (sama dengan sapod, ADR 0005). Batas jumlah disk per
// susunan datang dari server (agent.storage.pools.layouts).
import type { Disk, LayoutId, PoolLayout } from "./api"

// Jumlah disk yang boleh rusak tanpa kehilangan data.
export function faultTolerance(layout: LayoutId, disks: number): number {
  switch (layout) {
    case "mirror":
      return Math.max(0, disks - 1)
    case "raidz1":
      return 1
    case "raidz2":
      return 2
    case "raidz3":
      return 3
    default:
      return 0
  }
}

// Perkiraan kapasitas pakai sebelum overhead ZFS: setiap disk terpakai
// sebesar disk terkecil.
export function usableBytes(layout: LayoutId, sizes: number[]): number {
  if (!sizes.length) return 0
  const smallest = Math.min(...sizes)
  const n = sizes.length
  switch (layout) {
    case "single":
    case "mirror":
      return smallest
    default:
      return Math.max(0, n - faultTolerance(layout, n)) * smallest
  }
}

export function fits(l: PoolLayout, disks: number): boolean {
  return disks >= l.min_disks && disks <= l.max_disks
}

// Susunan yang disarankan untuk jumlah disk ini: redundansi lebih dulu,
// lalu kapasitas. Empat disk atau lebih: tahan dua disk rusak (raidz2),
// karena membangun ulang disk besar lama dan bisa memicu disk kedua rusak.
export function recommended(layouts: PoolLayout[], disks: number): LayoutId | "" {
  const order: LayoutId[] = disks >= 4 ? ["raidz2", "raidz1", "mirror"] : disks === 3 ? ["raidz1", "mirror"] : disks === 2 ? ["mirror"] : ["single"]
  return order.find((id) => layouts.some((l) => l.id === id && fits(l, disks))) ?? ""
}

// Alasan disk tidak bisa dipilih (kunci i18n), atau "" bila boleh.
// all: semua disk (nomor seri atau WWN yang sama di disk lain berarti disk
// tidak bisa dipastikan, ADR 0015).
export function ineligibleReason(d: Disk, allowRemovable: boolean, allowUSB = false, all: Disk[] = []): string {
  if (d.usage === "in_use") return `storage.usage.${d.usage_reason ?? "mounted"}`
  if (d.transport === "usb" && !allowUSB) return "storage.usage.usb"
  if (d.removable && !allowRemovable) return "storage.usage.removable"
  if (all.some((x) => x.name !== d.name && ((d.serial && x.serial === d.serial) || (d.wwn && x.wwn === d.wwn)))) return "storage.usage.identity"
  return ""
}
