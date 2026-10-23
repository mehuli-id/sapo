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

export interface UIConfig {
  default_mode: "desktop" | "ringkas"
  default_language: string
  branding?: Branding
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

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message)
  }
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Accept: "application/json" }, credentials: "same-origin" })
  if (!res.ok) {
    let code = "http_error"
    let message = res.statusText
    try {
      const body = await res.json()
      code = body?.error?.code ?? code
      message = body?.error?.message ?? message
    } catch {
      // badan respons bukan JSON
    }
    throw new ApiError(res.status, code, message)
  }
  return res.json() as Promise<T>
}

export const api = {
  systemInfo: () => get<SystemInfo>("/api/v1/system/info"),
  features: () => get<Feature[]>("/api/v1/features"),
  uiConfig: () => get<UIConfig>("/api/v1/ui/config"),
}
