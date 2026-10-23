import type { Localized } from "./api"

// Memilih teks dari data berbahasa ganda (YAML wizard, manifest fitur).
export function pick(text: Localized | undefined, locale: string): string {
  if (!text) return ""
  return text[locale] ?? text.en ?? text.id ?? Object.values(text)[0] ?? ""
}
