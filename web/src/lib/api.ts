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
  // Admin sengaja memakai disk USB (ADR 0015).
  allow_usb?: boolean
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
  // Aplikasi App Center yang diberi akses → read | write.
  apps?: Record<string, ShareAccess>
  // Jadwal snapshot (tidak ada = mati).
  snapshots?: SnapshotPolicy
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

export interface StatsRate {
  name: string
  // byte per detik: diterima/dibaca dan dikirim/ditulis
  in: number
  out: number
}

export interface StatsSample {
  // nomor urut, dipakai untuk meminta sampel berikutnya
  seq: number
  time: string
  cpu_percent: number
  iowait_percent: number
  memory_used_bytes: number
  memory_total_bytes: number
  net: StatsRate[]
  disk: StatsRate[]
}

export interface SystemStats {
  interval_seconds: number
  cpu_cores: number
  samples: StatsSample[]
}

export interface FileShare {
  name: string
  description?: string
  access: ShareAccess
  // Folder pribadi pengguna (Sapo Drive), bernama "@home".
  home?: boolean
  // Folder eksternal (USB).
  external?: boolean
}

// Password aplikasi untuk WebDAV (Sapo Drive); password-nya hanya
// ditampilkan sekali saat dibuat.
export interface AppPassword {
  id: string
  name: string
  created_at: string
  last_used_at?: string
}

// Penyimpanan eksternal (USB, ADR 0015).
export interface ExternalVolume {
  uuid: string
  device: string
  disk: string
  model: string
  serial: string
  fstype: string
  label: string
  size_bytes: number
  supported: boolean
  reason?: string
  name?: string
  read_only?: boolean
  group?: string
  smb?: boolean
  state: "available" | "mounted" | "disconnected" | "error"
  error?: string
  used_bytes?: number
  available_bytes?: number
}

export interface HomeStatus {
  pool: string
  user: string
  used_bytes: number
  available_bytes: number
  // 0 = tanpa batas.
  quota_bytes: number
}

export interface DriveInfo {
  enabled: boolean
  pool: string
  smb: boolean
  webdav: boolean
  user: string
  home?: HomeStatus
}

export interface DriveAdmin {
  pool: string
  quota_gb: number
  // quota_gb: kuota khusus (-1 = tanpa batas); tidak ada = bawaan.
  users: { user: string; exists: boolean; quota_gb?: number; home?: HomeStatus }[]
  // Folder pengguna yang sudah dihapus, atau di pool lain.
  orphans: HomeStatus[]
  // Jadwal snapshot semua folder pribadi (tidak ada = mati).
  snapshots?: SnapshotPolicy | null
}

// Snapshot ZFS terjadwal (ADR 0014): jumlah snapshot per jam, harian, dan
// mingguan yang disimpan; hour = jam snapshot harian/mingguan (waktu NAS).
export interface SnapshotPolicy {
  hourly: number
  daily: number
  weekly: number
  hour: number
}

export interface SnapshotPreset extends SnapshotPolicy {
  id: string
  label: Localized
}

export interface SnapshotInfo {
  name: string
  kind: "hourly" | "daily" | "weekly" | "manual"
  created: string
  used_bytes: number
}

// Folder snapshot: shared folder (share) atau folder pribadi (user).
export interface SnapshotTarget {
  pool: string
  share?: string
  user?: string
}

export interface FileEntry {
  name: string
  type: "dir" | "file" | "symlink" | "other"
  size: number
  mtime: string
}

export interface FileList {
  entries: FileEntry[]
  truncated?: boolean
}

// snapshot: versi sebelumnya (ADR 0014), baca saja.
const fileQuery = (share: string, path: string, snapshot = "") =>
  `share=${encodeURIComponent(share)}&path=${encodeURIComponent(path)}` + (snapshot ? `&snapshot=${encodeURIComponent(snapshot)}` : "")

// downloadURL: dibuka langsung oleh browser (lampiran), tidak lewat fetch.
export const fileDownloadURL = (share: string, path: string, snapshot = "") => `/api/v1/files/download?${fileQuery(share, path, snapshot)}`

// uploadFile mengunggah satu file lewat XMLHttpRequest (ada kemajuan).
export function uploadFile(share: string, dir: string, file: File, overwrite: boolean, onProgress: (loaded: number) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open("POST", `/api/v1/files/upload?${fileQuery(share, dir)}&name=${encodeURIComponent(file.name)}${overwrite ? "&overwrite=1" : ""}`)
    xhr.setRequestHeader("X-Sapo-Request", "1")
    xhr.setRequestHeader("Content-Type", "application/octet-stream")
    xhr.upload.onprogress = (e) => onProgress(e.loaded)
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) return resolve()
      let code = "http_error"
      let message = xhr.statusText
      try {
        const data = JSON.parse(xhr.responseText)
        code = data?.error?.code ?? code
        message = data?.error?.message ?? message
      } catch {
        // bukan JSON
      }
      reject(new ApiError(xhr.status, code, message, {}))
    }
    xhr.onerror = () => reject(new ApiError(0, "network_error", "network error", {}))
    xhr.send(file)
  })
}

export interface CatalogApp {
  id: string
  name: Localized
  description: Localized
  website?: string
  image: string
  default_port: number
  memory_mb: number
  // Shared folder dipasang di <share_path>/<nama> di dalam container.
  share_path?: string
  share_write?: boolean
}

export interface InstalledApp {
  id: string
  pool: string
  port: number
  installed: boolean
  enabled: boolean
  status?: string
  installed_at: string
  // Shared folder yang sedang dipasang di container.
  shares?: { name: string; write?: boolean }[]
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
  appCatalog: () => get<CatalogApp[]>("/api/v1/apps/catalog"),
  installedApps: () => get<InstalledApp[]>("/api/v1/apps"),
  installApp: (req: { id: string; pool: string; port: number; admin_password: string }) => post<{ ok: boolean }>("/api/v1/apps/install", req),
  startApp: (id: string) => post<{ ok: boolean }>(`/api/v1/apps/${encodeURIComponent(id)}/start`),
  stopApp: (id: string) => post<{ ok: boolean }>(`/api/v1/apps/${encodeURIComponent(id)}/stop`),
  removeApp: (id: string, req: { remove_data: boolean; confirm_name?: string; admin_password?: string }) =>
    post<{ ok: boolean; warning?: string }>(`/api/v1/apps/${encodeURIComponent(id)}/remove`, req),
  setAppFolders: (id: string, req: { folders: Record<string, ShareAccess>; admin_password: string }) =>
    post<{ ok: boolean; warning?: string }>(`/api/v1/apps/${encodeURIComponent(id)}/folders`, req),
  services: () => get<{ smb: boolean; nfs: boolean; webdav: boolean }>("/api/v1/settings/services"),
  setServices: (req: { smb?: boolean; nfs?: boolean; webdav?: boolean }) =>
    post<{ smb: boolean; nfs: boolean; webdav: boolean; warning?: string }>("/api/v1/settings/services", req),
  appPasswords: () => get<AppPassword[]>("/api/v1/account/app-passwords"),
  createAppPassword: (name: string, password: string) => post<{ id: string; name: string; password: string }>("/api/v1/account/app-passwords", { name, password }),
  revokeAppPassword: (id: string) => post<{ ok: boolean }>(`/api/v1/account/app-passwords/${encodeURIComponent(id)}/delete`),
  updatesStatus: () =>
    get<{
      running: boolean
      last?: { started_at: string; finished_at?: string; ok: boolean; exit_code: number; interrupted?: boolean }
      log: string[]
    }>("/api/v1/updates"),
  checkUpdates: () => post<{ checked_at: string; packages: { name: string; from?: string; to: string }[] }>("/api/v1/updates/check"),
  installUpdates: (admin_password: string) => post<{ ok: boolean }>("/api/v1/updates/install", { admin_password }),
  timeStatus: () => get<{ timezone: string; now: string; ntp: boolean; synchronized: boolean }>("/api/v1/settings/time"),
  timezones: () => get<{ zones: string[] }>("/api/v1/settings/timezones"),
  setTime: (timezone: string, ntp: boolean) => post<{ ok: boolean }>("/api/v1/settings/time", { timezone, ntp }),
  fileShares: () => get<FileShare[]>("/api/v1/files/shares"),
  drive: () => get<DriveInfo>("/api/v1/drive"),
  externalVolumes: () => get<ExternalVolume[]>("/api/v1/storage/external"),
  mountExternal: (req: { uuid: string; name: string; read_only: boolean; group: string; smb: boolean; admin_password: string }) =>
    post<{ ok: boolean }>("/api/v1/storage/external/mount", req),
  unmountExternal: (name: string) => post<{ ok: boolean }>(`/api/v1/storage/external/${encodeURIComponent(name)}/unmount`),
  snapshotPresets: () => get<SnapshotPreset[]>("/api/v1/snapshots/presets"),
  snapshots: (t: SnapshotTarget) =>
    get<{ snapshots: SnapshotInfo[]; used_by_snapshots: number }>(`/api/v1/snapshots?${new URLSearchParams({ pool: t.pool, share: t.share ?? "", user: t.user ?? "" })}`),
  createSnapshot: (t: SnapshotTarget) => post<{ name: string }>("/api/v1/snapshots", t),
  deleteSnapshot: (t: SnapshotTarget, name: string, admin_password: string) => post<{ ok: boolean }>("/api/v1/snapshots/delete", { ...t, name, admin_password }),
  // admin_password wajib bila jadwal baru akan membuang snapshot yang ada.
  setSharePolicy: (share: string, policy: SnapshotPolicy | null, admin_password = "") =>
    post<{ ok: boolean; warning?: string }>(`/api/v1/shares/${encodeURIComponent(share)}/snapshot-policy`, { policy, admin_password }),
  setDrivePolicy: (policy: SnapshotPolicy | null, admin_password = "") =>
    post<{ ok: boolean; warning?: string }>("/api/v1/drive/snapshot-policy", { policy, admin_password }),
  driveAdmin: () => get<DriveAdmin>("/api/v1/drive/admin"),
  setDrive: (req: { pool: string; quota_gb: number; admin_password: string }) => post<{ ok: boolean; warning?: string }>("/api/v1/drive/settings", req),
  setDriveQuota: (user: string, req: { quota_gb: number | null; admin_password: string }) =>
    post<{ ok: boolean; warning?: string }>(`/api/v1/drive/users/${encodeURIComponent(user)}/quota`, req),
  deleteHome: (pool: string, user: string, req: { confirm_name: string; admin_password: string }) =>
    post<{ ok: boolean }>(`/api/v1/drive/homes/${encodeURIComponent(pool)}/${encodeURIComponent(user)}/delete`, req),
  listFiles: (share: string, path: string, snapshot = "") => get<FileList>(`/api/v1/files/list?${fileQuery(share, path, snapshot)}`),
  fileVersions: (share: string) => get<{ name: string; kind: string; created: string }[]>(`/api/v1/files/snapshots?share=${encodeURIComponent(share)}`),
  restoreFile: (share: string, path: string, snapshot: string, overwrite: boolean) =>
    post<{ name: string; files: number; bytes: number; skipped?: number }>("/api/v1/files/restore", { share, path, snapshot, overwrite }),
  mkdir: (share: string, path: string) => post<{ ok: boolean }>("/api/v1/files/mkdir", { share, path }),
  renameFile: (share: string, path: string, to: string) => post<{ ok: boolean }>("/api/v1/files/rename", { share, path, to }),
  deleteFile: (share: string, path: string) => post<{ ok: boolean }>("/api/v1/files/delete", { share, path }),
  systemStats: (after = 0) => get<SystemStats>(`/api/v1/system/stats?after=${after}`),
  applyNetwork: (config: NetworkConfig, admin_password: string) => post<NetworkApplyResult>("/api/v1/network", { config, admin_password }),
  confirmNetwork: (token: string) => post<{ ok: boolean }>("/api/v1/network/confirm", { token }),
  cancelNetwork: (token: string) => post<{ ok: boolean }>("/api/v1/network/cancel", { token }),
  scrubPool: (name: string, action: "start" | "stop") =>
    post<{ ok: boolean }>(`/api/v1/storage/pools/${encodeURIComponent(name)}/scrub`, { action }),
  replacePoolDisk: (name: string, req: { old_guid: string; disk: DiskRef; password: string; allow_usb?: boolean }) =>
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
      allow_usb: v.allow_usb === "yes",
    }),
  "storage.pool_replace": (v, ctx) => {
    const [disk] = diskRefs(splitList(v.disk), ctx, v.erase_ok === "yes")
    if (!ctx.pool || !ctx.vdev?.guid || !disk) throw new ApiError(409, "disk_changed", "")
    return api.replacePoolDisk(ctx.pool.name, { old_guid: ctx.vdev.guid, disk, password: v.password ?? "", allow_usb: v.allow_usb === "yes" })
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
  "drive.enable": async (v) => {
    const res = await api.setDrive({ pool: v.pool ?? "", quota_gb: Number(v.quota_gb || 0), admin_password: v.admin_password ?? "" })
    if (res.warning) throw new ApiError(200, res.warning, "")
    return res
  },
  "storage.pool_destroy": (v, ctx) => {
    if (!ctx.pool) throw new ApiError(404, "not_found", "")
    return api.destroyPool(ctx.pool.name, { confirm_name: v.confirm_name ?? "", password: v.password ?? "" })
  },
}
