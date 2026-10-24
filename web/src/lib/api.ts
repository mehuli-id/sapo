// Klien API panel. Semua data panel datang dari sapo-api; panel tidak pernah
// menyimpan nilai sistem di kode.

export interface SystemInfo {
  hostname: string
  os: string
  kernel: string
  arch: string
  uptime_seconds: number
  cpu: { model: string; cores: number }
  memory: { total_bytes: number; available_bytes: number }
  load_average: number[] | null
  sapo_version: string
}

export interface Feature {
  id: string
  name: Record<string, string>
  group: string
  packages: string[]
  services: string[]
  requires: string[]
  activation: "always" | "setup" | "optional"
  removable: boolean
}

export interface Partition {
  name: string
  size_bytes: number
  type: string
  fstype: string
  label: string
  mountpoints: string[]
}

export interface Disk {
  name: string
  path: string
  size_bytes: number
  model: string
  vendor: string
  serial: string
  wwn: string
  transport: string
  rotational: boolean
  removable: boolean
  fstype: string
  pttype: string
  partitions: Partition[]
  // Apakah disk boleh dipakai pool baru (dinilai sapod).
  usage: DiskUsage
  usage_reason?: "mounted" | "swap" | "pool" | "raid" | "lvm" | "crypt"
}

export type DiskUsage = "free" | "data" | "in_use"

export type LayoutId = "single" | "mirror" | "raidz1" | "raidz2" | "raidz3"

export interface PoolLayout {
  id: LayoutId
  min_disks: number
  max_disks: number
}

export interface Vdev {
  name: string
  type: string
  state: string
  path?: string
  disk?: string
  read_errors: number
  write_errors: number
  checksum_errors: number
  children?: Vdev[]
}

export interface PoolScan {
  function: "scrub" | "resilver" | string
  state: "scanning" | "finished" | "canceled" | string
  paused: boolean
  start_time: number
  end_time?: number
  examined_bytes: number
  to_examine_bytes: number
  issued_bytes: number
  errors: number
}

export interface Pool {
  name: string
  health: string
  size_bytes: number
  allocated_bytes: number
  free_bytes: number
  capacity_pct: number
  layout: LayoutId | "stripe" | "mixed" | ""
  status?: string
  action?: string
  error_count: number
  vdevs: Vdev[]
  scan?: PoolScan
}

export interface PoolsResult {
  // false: ZFS belum terpasang atau modul kernel tidak termuat (lihat message).
  available: boolean
  message?: string
  layouts: PoolLayout[]
  allow_removable: boolean
  pools: Pool[]
}

// Disk yang dipilih beserta identitas yang dilihat pengguna; sapod menolak
// bila disk sudah berbeda.
export interface DiskRef {
  name: string
  serial: string
  wwn: string
  size_bytes: number
  erase: boolean
}

export interface PoolCreateRequest {
  name: string
  layout: string
  disks: DiskRef[]
  confirm_name: string
  password: string
}

export type SmartStatus = "passed" | "warning" | "failed" | "standby" | "unsupported" | "unavailable"

export interface SmartAttr {
  id?: number
  name: string
  value?: number
  worst?: number
  threshold?: number
  raw: string
  failing: boolean
  warning: boolean
}

export interface Smart {
  device: string
  status: SmartStatus
  message?: string
  protocol?: string
  temperature_c?: number
  power_on_hours?: number
  attributes: SmartAttr[]
}

export interface AuthPolicy {
  password_min_length: number
  username_pattern: string
}

export interface HTTPSConfig {
  port: string
  fingerprint_sha256: string
}

export interface UIConfig {
  default_mode: "desktop" | "ringkas"
  default_language: string
  auth_policy: AuthPolicy
  branding?: Branding
  // Ada bila HTTPS wajib; lewat HTTP panel hanya menampilkan petunjuk.
  https?: HTTPSConfig
}

export interface Branding {
  product: {
    name: string
    full_name: string
    vendor: string
    website: string
    tagline: Record<string, string>
  }
  theme?: {
    radius?: string
    light?: Record<string, string>
    dark?: Record<string, string>
  }
}

export interface AuthStatus {
  setup_required: boolean
  authenticated: boolean
  user?: {
    username: string
    role: string
    // Ada bila 2FA aktif: sisa kode cadangan dan apakah sudah perlu dibuat ulang.
    recovery_codes_left?: number
    recovery_codes_low?: boolean
  }
}

export type Localized = Record<string, string>

export interface WizardField {
  key: string
  type:
    | "text"
    | "username"
    | "password"
    | "password-confirm"
    | "current-password"
    | "language"
    | "totp-enroll"
    | "recovery-codes"
    | "disk-select"
    | "pool-layout"
    | "erase-confirm"
    | "confirm"
  label: Localized
  help?: Localized
  placeholder?: Localized
  required?: boolean
  default?: string
  match?: string
  // Isian lain yang dipakai jenis isian ini (misalnya pool-layout → disk-select).
  source?: string
  // Pola isian teks (ekspresi reguler); pemeriksaan sebenarnya di server.
  pattern?: string
  autocomplete?: string
  secret?: boolean
}

export interface WizardStep {
  id: string
  type?: "form" | "review"
  title: Localized
  description?: Localized
  help?: Localized
  fields?: WizardField[]
}

export interface Wizard {
  id: string
  title: Localized
  action: string
  // Teks tombol terakhir (bawaan: Selesai) dan tanda bahwa tindakannya destruktif.
  finish_label?: Localized
  destructive?: boolean
  steps: WizardStep[]
}

export interface TOTPEnrollment {
  token: string
  secret: string
  uri: string
  // Kode cadangan 2FA; hanya diberikan sekali, saat dibuat.
  recovery_codes: string[]
}

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public detail: Record<string, unknown> = {},
  ) {
    super(message)
  }
}

// Dipanggil saat sesi habis, agar panel kembali ke halaman login.
let onUnauthenticated: (() => void) | null = null
export function setUnauthenticatedHandler(fn: () => void) {
  onUnauthenticated = fn
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" }
  if (method !== "GET") {
    // Header wajib untuk perlindungan CSRF (lihat internal/api).
    headers["X-Sapo-Request"] = "1"
    headers["Content-Type"] = "application/json"
  }
  const res = await fetch(path, {
    method,
    headers,
    credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) {
    let code = "http_error"
    let message = res.statusText
    let detail: Record<string, unknown> = {}
    try {
      const data = await res.json()
      code = data?.error?.code ?? code
      message = data?.error?.message ?? message
      detail = data?.error ?? {}
    } catch {
      // badan respons bukan JSON
    }
    if (res.status === 401 && code === "unauthenticated") onUnauthenticated?.()
    throw new ApiError(res.status, code, message, detail)
  }
  return res.json() as Promise<T>
}

const get = <T>(path: string) => request<T>("GET", path)
const post = <T>(path: string, body: unknown = {}) => request<T>("POST", path, body)

export const api = {
  systemInfo: () => get<SystemInfo>("/api/v1/system/info"),
  features: () => get<Feature[]>("/api/v1/features"),
  disks: () => get<Disk[]>("/api/v1/storage/disks"),
  diskSmart: (name: string) => get<Smart>(`/api/v1/storage/disks/${encodeURIComponent(name)}/smart`),
  pools: () => get<PoolsResult>("/api/v1/storage/pools"),
  createPool: (req: PoolCreateRequest) => post<Pool>("/api/v1/storage/pools", req),
  scrubPool: (name: string, action: "start" | "stop") =>
    post<{ ok: boolean }>(`/api/v1/storage/pools/${encodeURIComponent(name)}/scrub`, { action }),
  uiConfig: () => get<UIConfig>("/api/v1/ui/config"),
  authStatus: () => get<AuthStatus>("/api/v1/auth/status"),
  wizard: (id: string) => get<Wizard>(`/api/v1/wizards/${encodeURIComponent(id)}`),
  setupTOTP: (username: string) => post<TOTPEnrollment>("/api/v1/setup/totp", { username }),
  setup: (values: Record<string, string>) => post<{ ok: boolean }>("/api/v1/setup", values),
  login: (username: string, password: string, totp_code?: string) =>
    post<{ ok: boolean }>("/api/v1/auth/login", { username, password, totp_code: totp_code ?? "" }),
  logout: () => post<{ ok: boolean }>("/api/v1/auth/logout"),
  regenerateRecoveryCodes: (password: string) => post<{ recovery_codes: string[] }>("/api/v1/account/recovery-codes", { password }),
  totpBegin: (password: string) => post<TOTPEnrollment>("/api/v1/account/totp/begin", { password }),
  totpConfirm: (token: string, code: string) => post<{ ok: boolean }>("/api/v1/account/totp/confirm", { token, code }),
}

// Data yang dibutuhkan isian wizard tertentu (misalnya daftar disk untuk
// disk-select), disiapkan aplikasi yang membuka wizard.
export interface WizardContext {
  disks?: Disk[]
  pools?: PoolsResult
}

// Daftar nama dari isian disk-select (dipisah koma).
export const splitList = (v: string | undefined) => (v ?? "").split(",").filter(Boolean)

// Tindakan akhir wizard (nilai `action` di YAML) → pemanggilan API.
export const wizardActions: Record<string, (values: Record<string, string>, ctx: WizardContext) => Promise<unknown>> = {
  "setup.complete": (v) => api.setup(v),
  "storage.pool_create": (v, ctx) => {
    const byName = new Map((ctx.disks ?? []).map((d) => [d.name, d]))
    const disks = splitList(v.disks).map((name) => {
      const d = byName.get(name)
      if (!d) throw new ApiError(409, "disk_changed", name)
      // Konfirmasi di wizard berlaku untuk semua disk yang dipilih.
      return { name, serial: d.serial, wwn: d.wwn, size_bytes: d.size_bytes, erase: v.erase_ok === "yes" }
    })
    return api.createPool({ name: v.name ?? "", layout: v.layout ?? "", disks, confirm_name: v.confirm_name ?? "", password: v.password ?? "" })
  },
}
