import { computed, ref } from "vue"
import { useMediaQuery } from "@vueuse/core"
import type { Branding } from "@/lib/api"

export type DisplayMode = "desktop" | "ringkas"

const MODE_KEY = "sapo.ui.mode"

const mode = ref<DisplayMode>("desktop")
const branding = ref<Branding | undefined>()

// Mode tampilan dipilih per pengguna. Default untuk pengguna baru datang dari
// konfigurasi (ui.default_mode), bukan dari kode panel.
export function initMode(defaultMode: DisplayMode) {
  let saved: string | null = null
  try {
    saved = localStorage.getItem(MODE_KEY)
  } catch {
    // abaikan
  }
  mode.value = saved === "desktop" || saved === "ringkas" ? saved : defaultMode
}

export function useShell() {
  const isSmall = useMediaQuery("(max-width: 767px)")
  // Di HP dan layar kecil, tampilan selalu layar penuh (mode ringkas).
  const effectiveMode = computed<DisplayMode>(() => (isSmall.value ? "ringkas" : mode.value))

  function setMode(m: DisplayMode) {
    mode.value = m
    try {
      localStorage.setItem(MODE_KEY, m)
    } catch {
      // abaikan
    }
  }

  return { mode, effectiveMode, isSmall, setMode, branding }
}
