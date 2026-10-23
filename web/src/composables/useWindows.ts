import { computed, reactive } from "vue"
import type { AppDef } from "@/apps/registry"

export interface WinState {
  appId: string
  x: number
  y: number
  width: number
  height: number
  z: number
  minimized: boolean
  maximized: boolean
}

const DEFAULT_SIZE = { width: 520, height: 400 }
const GEOMETRY_KEY = "sapo.windows.geometry"

// Posisi & ukuran terakhir setiap jendela disimpan per pengguna di browser
// (kenyamanan saja; bila penyimpanan tidak tersedia, default dipakai).
function loadGeometry(): Record<string, Pick<WinState, "x" | "y" | "width" | "height">> {
  try {
    return JSON.parse(localStorage.getItem(GEOMETRY_KEY) ?? "{}")
  } catch {
    return {}
  }
}

function saveGeometry(all: Record<string, Pick<WinState, "x" | "y" | "width" | "height">>) {
  try {
    localStorage.setItem(GEOMETRY_KEY, JSON.stringify(all))
  } catch {
    // abaikan
  }
}

const state = reactive({
  windows: [] as WinState[],
  topZ: 10,
})

export function useWindows() {
  const focused = computed(() => {
    const visible = state.windows.filter((w) => !w.minimized)
    return visible.sort((a, b) => b.z - a.z)[0]?.appId
  })

  function open(app: AppDef, area: { width: number; height: number }) {
    const existing = state.windows.find((w) => w.appId === app.id)
    if (existing) {
      existing.minimized = false
      focus(app.id)
      return
    }
    const saved = loadGeometry()[app.id]
    const size = app.window ?? DEFAULT_SIZE
    const width = Math.min(saved?.width ?? size.width, area.width - 16)
    const height = Math.min(saved?.height ?? size.height, area.height - 16)
    const offset = (state.windows.length % 6) * 28
    const x = clamp(saved?.x ?? Math.round((area.width - width) / 2) + offset - 70, 0, Math.max(0, area.width - width))
    const y = clamp(saved?.y ?? Math.round((area.height - height) / 3) + offset, 0, Math.max(0, area.height - height))
    state.windows.push({ appId: app.id, x, y, width, height, z: ++state.topZ, minimized: false, maximized: false })
  }

  function close(appId: string) {
    const i = state.windows.findIndex((w) => w.appId === appId)
    if (i >= 0) state.windows.splice(i, 1)
  }

  function focus(appId: string) {
    const w = state.windows.find((x) => x.appId === appId)
    if (w) w.z = ++state.topZ
  }

  function toggleMinimize(appId: string) {
    const w = state.windows.find((x) => x.appId === appId)
    if (!w) return
    if (w.minimized || focused.value !== appId) {
      w.minimized = false
      focus(appId)
    } else {
      w.minimized = true
    }
  }

  function remember(w: WinState) {
    if (w.maximized) return
    const all = loadGeometry()
    all[w.appId] = { x: w.x, y: w.y, width: w.width, height: w.height }
    saveGeometry(all)
  }

  return { state, focused, open, close, focus, toggleMinimize, remember }
}

export function clamp(v: number, min: number, max: number) {
  return Math.min(Math.max(v, min), max)
}
