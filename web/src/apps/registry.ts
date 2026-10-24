import type { Component } from "vue"
import { computed, defineAsyncComponent } from "vue"
import {
  Activity,
  Cloud,
  FolderOpen,
  Gauge,
  HardDrive,
  Info,
  LayoutGrid,
  Package,
  Settings,
  ShieldCheck,
  UsersRound,
} from "@lucide/vue"
import { useSession } from "@/composables/useSession"
import appsData from "./apps.json"

// Daftar aplikasi panel adalah data (apps.json). Nantinya aplikasi dari
// App Center menambahkan entrinya sendiri lewat API tanpa mengubah kode ini.

export interface AppDef {
  id: string
  icon: string
  component: string
  status: "ready" | "planned"
  phase?: number
  window?: { width: number; height: number }
  keywords?: string[]
  // Hanya tampil untuk administrator (server tetap memeriksa haknya).
  admin_only?: boolean
}

export const apps: AppDef[] = appsData as AppDef[]

// Aplikasi yang tampil untuk user yang sedang login.
export function useVisibleApps() {
  const { status } = useSession()
  return computed(() => apps.filter((a) => !a.admin_only || status.value?.user?.role === "admin"))
}

export function findApp(id: string): AppDef | undefined {
  return apps.find((a) => a.id === id)
}

// Peta nama ikon (di data) ke komponen ikon Lucide.
const icons: Record<string, Component> = {
  "activity": Activity,
  "cloud": Cloud,
  "folder-open": FolderOpen,
  "gauge": Gauge,
  "hard-drive": HardDrive,
  "info": Info,
  "layout-grid": LayoutGrid,
  "package": Package,
  "settings": Settings,
  "shield-check": ShieldCheck,
  "users-round": UsersRound,
}

export function iconFor(name: string): Component {
  return icons[name] ?? LayoutGrid
}

// Peta nama komponen (di data) ke modul aplikasi, dimuat saat dibuka.
const components: Record<string, Component> = {
  SystemInfo: defineAsyncComponent(() => import("./SystemInfo.vue")),
  Features: defineAsyncComponent(() => import("./Features.vue")),
  Storage: defineAsyncComponent(() => import("./Storage.vue")),
  AccountSecurity: defineAsyncComponent(() => import("./AccountSecurity.vue")),
  Users: defineAsyncComponent(() => import("./Users.vue")),
  About: defineAsyncComponent(() => import("./About.vue")),
  Planned: defineAsyncComponent(() => import("./Planned.vue")),
}

export function componentFor(app: AppDef): Component {
  return components[app.component] ?? components.Planned
}
