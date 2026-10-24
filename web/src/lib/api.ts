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
  label: string
  partitions: Partition[]
  // Apakah disk boleh dipakai pool baru (dinilai sapod).
  usage: DiskUsage
  // Untuk "data": zfs_label = label pool ZFS yang tidak aktif (pool terhapus, NAS lain).
  usage_reason?: "mounted" | "swap" | "pool" | "raid" | "lvm" | "crypt" | "holder" | "zfs_label"
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
  guid?: string
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
  usage: DiskUsage
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
    role: "admin" | "user"
    // Langkah wajib sebelum panel bisa dipakai: ganti password (dibuat admin)
    // dan 2FA (administrator).
    pending: ("password" | "totp")[]
    // Ada bila 2FA aktif: sisa kode cadangan dan apakah sudah perlu dibuat ulang.
    recovery_codes_left?: number
    recovery_codes_low?: boolean
  }
}

export type Role = "admin" | "user"

export interface UserAccount {
  username: string
  role: Role
  description: string
  disabled: boolean
  groups: string[]
  two_factor: boolean
  must_change_password: boolean
  created_at: string
  last_login_at?: string
  // Langkah wajib yang belum diselesaikan pemilik akun.
  pending: ("password" | "totp")[]
}

export interface UserGroup {
  name: string
  description: string
  members: string[]
  created_at: string
}

export type ShareAccess = "read" | "write"

export interface SharedFolder {
  name: string
  pool: string
  description?: string
  // "user:nama" / "group:nama" → read | write
  permissions: Record<string, ShareAccess>
  created_at: string
  path?: string
  error?: string
  used_bytes: number
  available_bytes: number
  // Izin sedang diterapkan ke isi folder / gagal diterapkan.
  acl_pending?: boolean
  acl_error?: string
  // Ekspor NFS: semua akses dari klien berlaku sebagai pengguna nfs.user.
  nfs?: ShareNFS
  // Izin efektif pengguna NFS (kosong = tidak diekspor) dan galat ekspor.
  nfs_access?: ShareAccess
  nfs_error?: string
}

export interface ShareNFS {
  user: string
  // Alamat IP atau jaringan, misalnya 192.168.1.0/24.
  clients: string[]
}

export interface NetworkInterfaceConfig {
  name: string
  // off: tidak dipakai (misalnya port tanpa kabel)
  mode: "dhcp" | "static" | "off"
  // IPv4 dengan prefiks, misalnya 192.168.1.10/24
  address?: string
  gateway?: string
}

export interface NetworkConfig {
  hostname: string
  interfaces: NetworkInterfaceConfig[]
  dns: string[]
}

export interface NetworkStatus {
  hostname: string
  // Pengaturan terakhir yang dikonfirmasi (kosong = belum diatur dari panel).
  config?: NetworkConfig
  interfaces: { name: string; mac: string; up: boolean; speed_mbps?: number; addresses: string[] }[]
  gateway?: string
  dns: string[]
  pending?: { confirm_by: string; config: NetworkConfig }
}

export interface NetworkApplyResult {
  token: string
  applies_at: string
  confirm_by: string
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
    | "acknowledge"
    | "choice"
    | "group-select"
    | "toggle"
    | "pool-select"
    | "permissions"
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
  // confirm: harus sama dengan nilai dari konteks wizard (misalnya nama pool).
  match_context?: "pool"
  // disk-select: jumlah disk paling banyak (1 = pilih satu).
  max?: number
  // choice: pilihan beserta labelnya.
  options?: { value: string; label: Localized; help?: Localized }[]
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
  networkStatus: () => get<NetworkStatus>("/api/v1/network"),
  applyNetwork: (config: NetworkConfig, admin_password: string) => post<NetworkApplyResult>("/api/v1/network", { config, admin_password }),
  confirmNetwork: (token: string) => post<{ ok: boolean }>("/api/v1/network/confirm", { token }),
  cancelNetwork: (token: string) => post<{ ok: boolean }>("/api/v1/network/cancel", { token }),
  scrubPool: (name: string, action: "start" | "stop") =>
    post<{ ok: boolean }>(`/api/v1/storage/pools/${encodeURIComponent(name)}/scrub`, { action }),
  replacePoolDisk: (name: string, req: { old_guid: string; disk: DiskRef; password: string }) =>
    post<{ ok: boolean }>(`/api/v1/storage/pools/${encodeURIComponent(name)}/replace`, req),
  destroyPool: (name: string, req: { confirm_name: string; password: string }) =>
    post<{ ok: boolean }>(`/api/v1/storage/pools/${encodeURIComponent(name)}/destroy`, req),
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
  changePassword: (current_password: string, password: string, password_confirm: string) =>
    post<{ ok: boolean }>("/api/v1/account/password", { current_password, password, password_confirm }),
  users: () => get<UserAccount[]>("/api/v1/users"),
  groups: () => get<UserGroup[]>("/api/v1/groups"),
  createUser: (req: Record<string, unknown>) => post<UserAccount>("/api/v1/users", req),
  updateUser: (name: string, req: { role?: Role; disabled?: boolean; groups?: string[]; description?: string; admin_password?: string }) =>
    post<{ user: UserAccount; warning?: string }>(`/api/v1/users/${encodeURIComponent(name)}`, req),
  resetUserPassword: (name: string, req: { password: string; password_confirm: string; must_change_password: boolean; admin_password: string }) =>
    post<{ ok: boolean }>(`/api/v1/users/${encodeURIComponent(name)}/password`, req),
  deleteUser: (name: string, req: { confirm_name: string; admin_password: string }) =>
    post<{ ok: boolean }>(`/api/v1/users/${encodeURIComponent(name)}/delete`, req),
  createGroup: (name: string, description: string) => post<{ ok: boolean }>("/api/v1/groups", { name, description }),
  updateGroup: (name: string, req: { description?: string; members?: string[]; admin_password?: string }) =>
    post<{ ok: boolean }>(`/api/v1/groups/${encodeURIComponent(name)}`, req),
  deleteGroup: (name: string) => post<{ ok: boolean }>(`/api/v1/groups/${encodeURIComponent(name)}/delete`),
  shares: () => get<SharedFolder[]>("/api/v1/shares"),
  syncStatus: () => get<{ ok: boolean; error: string; retrying: boolean }>("/api/v1/system/sync"),
  createShare: (req: { name: string; pool: string; description: string; permissions: Record<string, ShareAccess>; admin_password: string }) =>
    post<SharedFolder>("/api/v1/shares", req),
  updateShare: (
    name: string,
    req: { description?: string; permissions?: Record<string, ShareAccess>; nfs?: ShareNFS | null; admin_password?: string },
  ) =>
    post<{ ok: boolean; warning?: string }>(`/api/v1/shares/${encodeURIComponent(name)}`, req),
  deleteShare: (name: string, req: { confirm_name: string; admin_password: string }) =>
    post<{ ok: boolean }>(`/api/v1/shares/${encodeURIComponent(name)}/delete`, req),
}

// Data yang dibutuhkan isian wizard tertentu (misalnya daftar disk untuk
// disk-select), disiapkan aplikasi yang membuka wizard.
export interface WizardContext {
  disks?: Disk[]
  pools?: PoolsResult
  // Pool dan disk yang sedang dikerjakan (ganti disk, hapus pool).
  pool?: Pool
  vdev?: Vdev
  // Grup yang bisa dipilih (group-select, permissions).
  groups?: UserGroup[]
  // Pengguna untuk isian permissions.
  users?: UserAccount[]
}

// Daftar nama dari isian disk-select (dipisah koma).
export const splitList = (v: string | undefined) => (v ?? "").split(",").filter(Boolean)

// Tindakan akhir wizard (nilai `action` di YAML) → pemanggilan API.
// Isian permissions disimpan sebagai teks "user:budi=write,group:x=read".
export function parsePermissions(v: string | undefined): Record<string, ShareAccess> {
  const out: Record<string, ShareAccess> = {}
  for (const part of splitList(v)) {
    const [who, level] = part.split("=")
    if (who && (level === "read" || level === "write")) out[who] = level
  }
  return out
}

export function formatPermissions(p: Record<string, ShareAccess>): string {
  return Object.entries(p)
    .map(([who, level]) => `${who}=${level}`)
    .sort()
    .join(",")
}

// diskRefs menyusun identitas disk yang dipilih (seperti yang dilihat
// pengguna). Konfirmasi penghapusan hanya untuk disk yang tampil berisi data
// saat dipilih; disk lain dikirim tanpa izin hapus, sehingga sapod menolak
// bila ternyata kini berisi data.
function diskRefs(names: string[], ctx: WizardContext, eraseOk: boolean): DiskRef[] {
  const byName = new Map((ctx.disks ?? []).map((d) => [d.name, d]))
  return names.map((name) => {
    const d = byName.get(name)
    if (!d) throw new ApiError(409, "disk_changed", name)
    return { name, serial: d.serial, wwn: d.wwn, size_bytes: d.size_bytes, usage: d.usage, erase: eraseOk && d.usage === "data" }
  })
}

// Tindakan akhir wizard (nilai `action` di YAML) → pemanggilan API.
export const wizardActions: Record<string, (values: Record<string, string>, ctx: WizardContext) => Promise<unknown>> = {
  "setup.complete": (v) => api.setup(v),
  "storage.pool_create": (v, ctx) =>
    api.createPool({
      name: v.name ?? "",
      layout: v.layout ?? "",
      disks: diskRefs(splitList(v.disks), ctx, v.erase_ok === "yes"),
      confirm_name: v.confirm_name ?? "",
      password: v.password ?? "",
    }),
  "storage.pool_replace": (v, ctx) => {
    const [disk] = diskRefs(splitList(v.disk), ctx, v.erase_ok === "yes")
    if (!ctx.pool || !ctx.vdev?.guid || !disk) throw new ApiError(409, "disk_changed", "")
    return api.replacePoolDisk(ctx.pool.name, { old_guid: ctx.vdev.guid, disk, password: v.password ?? "" })
  },
  "users.create": (v) =>
    api.createUser({
      username: v.username ?? "",
      description: v.description ?? "",
      role: v.role ?? "user",
      groups: splitList(v.groups),
      password: v.password ?? "",
      password_confirm: v.password_confirm ?? "",
      must_change_password: v.must_change_password === "yes",
      admin_password: v.admin_password ?? "",
    }),
  "shares.create": (v) =>
    api.createShare({
      name: v.name ?? "",
      pool: v.pool ?? "",
      description: v.description ?? "",
      permissions: parsePermissions(v.permissions),
      admin_password: v.admin_password ?? "",
    }),
  "storage.pool_destroy": (v, ctx) => {
    if (!ctx.pool) throw new ApiError(404, "not_found", "")
    return api.destroyPool(ctx.pool.name, { confirm_name: v.confirm_name ?? "", password: v.password ?? "" })
  },
}
