import type { Branding } from "./api"

// Memasang warna dari branding.yaml sebagai CSS variable shadcn.
// Kunci snake_case di YAML (primary_foreground) menjadi --primary-foreground.
export function applyBranding(branding: Branding | undefined) {
  if (!branding?.theme) return
  const { radius, light, dark } = branding.theme
  const lines: string[] = []
  const toVars = (obj?: Record<string, string>) =>
    Object.entries(obj ?? {})
      .map(([k, v]) => `--${k.replace(/_/g, "-")}: ${v};`)
      .join(" ")
  lines.push(`:root { ${radius ? `--radius: ${radius};` : ""} ${toVars(light)} }`)
  lines.push(`.dark { ${toVars(dark)} }`)

  let el = document.getElementById("sapo-branding") as HTMLStyleElement | null
  if (!el) {
    el = document.createElement("style")
    el.id = "sapo-branding"
    document.head.appendChild(el)
  }
  el.textContent = lines.join("\n")
  if (branding.product?.name) document.title = branding.product.name
}

export function useColorScheme() {
  const key = "sapo.theme"
  const read = (): "light" | "dark" | "system" => {
    try {
      const v = localStorage.getItem(key)
      if (v === "light" || v === "dark") return v
    } catch {
      // penyimpanan browser tidak tersedia
    }
    return "system"
  }
  const apply = (mode: "light" | "dark" | "system") => {
    const dark = mode === "dark" || (mode === "system" && window.matchMedia("(prefers-color-scheme: dark)").matches)
    document.documentElement.classList.toggle("dark", dark)
  }
  const set = (mode: "light" | "dark" | "system") => {
    try {
      localStorage.setItem(key, mode)
    } catch {
      // abaikan
    }
    apply(mode)
  }
  return { read, apply, set }
}
