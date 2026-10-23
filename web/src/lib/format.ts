// Format angka mengikuti bahasa aktif (Intl), tanpa teks yang ditulis di kode.

// base 1024 untuk memori dan file; 1000 untuk kapasitas disk, agar sama
// dengan label pabrik (disk "4 TB" = 4.000.787.030.016 byte).
export function formatBytes(bytes: number, locale: string, base: 1000 | 1024 = 1024): string {
  const units = ["byte", "kilobyte", "megabyte", "gigabyte", "terabyte", "petabyte"] as const
  let value = bytes
  let i = 0
  while (value >= base && i < units.length - 1) {
    value /= base
    i++
  }
  return new Intl.NumberFormat(locale, {
    style: "unit",
    unit: units[i],
    unitDisplay: "short",
    maximumFractionDigits: value < 10 && i > 0 ? 1 : 0,
  }).format(value)
}

export function formatNumber(n: number, locale: string, digits = 2): string {
  return new Intl.NumberFormat(locale, { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(n)
}

export function splitDuration(seconds: number) {
  const s = Math.max(0, Math.floor(seconds))
  return { days: Math.floor(s / 86400), hours: Math.floor((s % 86400) / 3600), minutes: Math.floor((s % 3600) / 60) }
}
