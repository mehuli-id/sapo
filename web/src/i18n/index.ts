import { createI18n } from "vue-i18n"
import en from "./en.json"
import id from "./id.json"

// Teks UI hanya berasal dari file terjemahan ini, tidak ditulis di komponen.
export const languages = [
  { code: "id", label: "Bahasa Indonesia" },
  { code: "en", label: "English" },
] as const

function initialLocale(): string {
  try {
    const saved = localStorage.getItem("sapo.locale")
    if (saved && languages.some((l) => l.code === saved)) return saved
  } catch {
    // penyimpanan browser tidak tersedia
  }
  return "id"
}

export const i18n = createI18n({
  legacy: false,
  locale: initialLocale(),
  fallbackLocale: "en",
  messages: { id, en },
})

export function setLocale(code: string) {
  i18n.global.locale.value = code as "id" | "en"
  document.documentElement.lang = code
  try {
    localStorage.setItem("sapo.locale", code)
  } catch {
    // abaikan
  }
}
