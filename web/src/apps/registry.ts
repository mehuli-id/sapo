import type { Component } from "vue"
import { defineAsyncComponent } from "vue"
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
} from "@lucide/vue"
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
}

export const apps: AppDef[] = appsData as AppDef[]

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
}

export function iconFor(name: string): Component {
  return icons[name] ?? LayoutGrid
}

// Peta nama komponen (di data) ke modul aplikasi, dimuat saat dibuka.
const components: Record<string, Component> = {
  SystemInfo: defineAsyncComponent(() => import("./SystemInfo.vue")),
  Features: defineAsyncComponent(() => import("./Features.vue")),
  Storage: defineAsyncComponent(() => import("./Storage.vue")),
  About: defineAsyncComponent(() => import("./About.vue")),
  Planned: defineAsyncComponent(() => import("./Planned.vue")),
}

export function componentFor(app: AppDef): Component {
  return components[app.component] ?? components.Planned
}
