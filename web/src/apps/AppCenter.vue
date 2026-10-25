<script setup lang="ts">
// App Center (hanya admin, ADR 0012): katalog aplikasi kurasi, pasang di
// pool, jalankan/hentikan, buka, beri akses shared folder, dan hapus (data
// disimpan kecuali dipilih dihapus juga).
import { computed, onMounted, onUnmounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, ExternalLink, FolderOpen, LayoutGrid, LoaderCircle, Play, Square, Trash2, TriangleAlert } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import type { CatalogApp, InstalledApp, PoolsResult, SharedFolder, ShareAccess } from "@/lib/api"
import { api, ApiError } from "@/lib/api"
import { cn } from "@/lib/utils"

const { t, locale } = useI18n()
const catalog = ref<CatalogApp[] | null>(null)
const installed = ref<InstalledApp[]>([])
const pools = ref<PoolsResult | null>(null)
const shares = ref<SharedFolder[]>([])
const error = ref("")
const notice = ref("")
const busy = ref(false)

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`, { message: e instanceof Error ? e.message : "" }) : t("errors.generic")
}
const loc = (m: Record<string, string>) => m[locale.value] ?? m.en ?? Object.values(m)[0] ?? ""

async function load() {
  try {
    ;[catalog.value, installed.value, pools.value, shares.value] = await Promise.all([api.appCatalog(), api.installedApps(), api.pools(), api.shares()])
  } catch (e) {
    error.value = message(e)
  }
}
// Status aplikasi (misalnya image masih diunduh) diperbarui berkala.
let timer: number | undefined
onMounted(() => {
  load()
  timer = window.setInterval(async () => {
    try {
      installed.value = await api.installedApps()
    } catch {
      // abaikan
    }
  }, 5000)
})
onUnmounted(() => window.clearInterval(timer))

const byId = computed(() => Object.fromEntries(installed.value.map((a) => [a.id, a])))
const appURL = (a: InstalledApp) => `http://${window.location.hostname}:${a.port}/`

// --- Pasang ---
const mode = ref("") // "install:<id>" | "remove:<id>" | "folders:<id>"
const form = reactive({ pool: "", port: 0, password: "", removeData: false, confirm: "", folders: {} as Record<string, ShareAccess | "none"> })
function start(m: string, app?: CatalogApp) {
  mode.value = m
  error.value = notice.value = ""
  const inst = app ? byId.value[app.id] : undefined
  Object.assign(form, {
    pool: inst?.pool ?? pools.value?.pools[0]?.name ?? "",
    port: app?.default_port ?? 0,
    password: "",
    removeData: false,
    confirm: "",
    folders: Object.fromEntries(shares.value.map((sh) => [sh.name, (app && sh.apps?.[app.id]) || "none"])),
  })
}
async function run(fn: () => Promise<unknown>, done: string) {
  busy.value = true
  error.value = notice.value = ""
  try {
    await fn()
    notice.value = done
    mode.value = ""
    ;[installed.value, shares.value] = await Promise.all([api.installedApps(), api.shares()])
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}
const install = (app: CatalogApp) =>
  run(() => api.installApp({ id: app.id, pool: form.pool, port: Number(form.port), admin_password: form.password }), t("appCenter.installed", { name: loc(app.name) }))
const remove = (app: CatalogApp) =>
  run(
    () => api.removeApp(app.id, form.removeData ? { remove_data: true, confirm_name: form.confirm, admin_password: form.password } : { remove_data: false }),
    t(form.removeData ? "appCenter.removedData" : "appCenter.removed", { name: loc(app.name) }),
  )
// Folder yang diberikan (keadaan tersimpan di panel).
const appFolders = (id: string) =>
  shares.value.filter((sh) => sh.apps?.[id]).map((sh) => `${sh.name} (${t(`permissions.${sh.apps![id]}`)})`)
async function saveFolders(app: CatalogApp) {
  const folders = Object.fromEntries(Object.entries(form.folders).filter(([, l]) => l !== "none")) as Record<string, ShareAccess>
  await run(async () => {
    const res = await api.setAppFolders(app.id, { folders, admin_password: form.password })
    if (res.warning) throw new ApiError(200, res.warning, "")
  }, t("appCenter.foldersSaved", { name: loc(app.name) }))
}
const toggle = (app: CatalogApp, on: boolean) =>
  run(() => (on ? api.startApp(app.id) : api.stopApp(app.id)), t(on ? "appCenter.started" : "appCenter.stopped", { name: loc(app.name) }))

function statusText(a: InstalledApp) {
  if (!a.installed) return t("appCenter.dataKept")
  if (!a.enabled) return t("appCenter.status.stopped")
  return t(`appCenter.status.${a.status === "active" ? "running" : a.status === "failed" ? "failed" : "starting"}`)
}
</script>

<template>
  <div class="space-y-4 p-4">
    <p class="text-sm text-muted-foreground">{{ t("appCenter.intro") }}</p>
    <p v-if="pools && !pools.pools.length" class="rounded-lg bg-muted p-3 text-sm">{{ t("shares.noPool") }}</p>
    <p v-if="notice" class="flex items-center gap-2 rounded-lg border border-emerald-500/40 bg-emerald-500/5 p-3 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ notice }}</p>
    <p v-if="error" class="flex items-center gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm" role="alert"><TriangleAlert class="size-4" />{{ error }}</p>

    <ul v-if="catalog" class="space-y-3">
      <li v-for="app in catalog" :key="app.id" class="space-y-3 rounded-xl border p-4">
        <div class="flex flex-wrap items-start justify-between gap-2">
          <div class="flex min-w-0 items-start gap-3">
            <LayoutGrid class="mt-0.5 size-5 shrink-0 text-muted-foreground" />
            <div class="min-w-0 space-y-0.5">
              <p class="font-medium">{{ loc(app.name) }}</p>
              <p class="text-xs text-muted-foreground">{{ loc(app.description) }}</p>
              <p v-if="byId[app.id]" :class="cn('text-xs', byId[app.id]!.status === 'failed' && byId[app.id]!.enabled ? 'text-destructive' : '')">
                {{ statusText(byId[app.id]!) }} · {{ t("appCenter.onPool", { pool: byId[app.id]!.pool, port: byId[app.id]!.port }) }}
              </p>
              <p v-if="app.share_path && byId[app.id]?.installed" class="text-xs">
                {{ t("appCenter.foldersSummary", { list: appFolders(app.id).join(", ") || t("appCenter.foldersNone") }) }}
              </p>
            </div>
          </div>
          <div v-if="!mode.endsWith(`:${app.id}`)" class="flex flex-wrap gap-1">
            <template v-if="byId[app.id]?.installed">
              <a v-if="byId[app.id]!.enabled && byId[app.id]!.status === 'active'" :href="appURL(byId[app.id]!)" target="_blank" rel="noopener noreferrer">
                <Button size="sm"><ExternalLink />{{ t("appCenter.open") }}</Button>
              </a>
              <Button v-if="app.share_path" size="sm" variant="outline" @click="start(`folders:${app.id}`, app)"><FolderOpen />{{ t("appCenter.folders") }}</Button>
              <Button v-if="byId[app.id]!.enabled" size="sm" variant="outline" :disabled="busy" @click="toggle(app, false)"><Square />{{ t("appCenter.stop") }}</Button>
              <Button v-else size="sm" variant="outline" :disabled="busy" @click="toggle(app, true)"><Play />{{ t("appCenter.start") }}</Button>
              <Button size="sm" variant="ghost" class="text-destructive hover:text-destructive" @click="start(`remove:${app.id}`, app)"><Trash2 /></Button>
            </template>
            <template v-else>
              <Button size="sm" :disabled="!pools?.pools.length" @click="start(`install:${app.id}`, app)">{{ t("appCenter.install") }}</Button>
              <Button v-if="byId[app.id]" size="sm" variant="ghost" class="text-destructive hover:text-destructive" @click="start(`remove:${app.id}`, app)"><Trash2 /></Button>
            </template>
          </div>
        </div>

        <form v-if="mode === `install:${app.id}`" class="space-y-3 border-t pt-3" @submit.prevent="install(app)">
          <p class="text-xs text-muted-foreground">{{ t("appCenter.installHelp", { memory: app.memory_mb }) }}</p>
          <div class="grid gap-3 sm:grid-cols-2">
            <div class="space-y-1.5">
              <Label :for="`ap-pool-${app.id}`">{{ t("appCenter.pool") }}</Label>
              <select
                :id="`ap-pool-${app.id}`"
                v-model="form.pool"
                :disabled="!!byId[app.id]"
                class="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm shadow-xs focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
              >
                <option v-for="p in pools?.pools ?? []" :key="p.name" :value="p.name">{{ p.name }}</option>
              </select>
            </div>
            <div class="space-y-1.5">
              <Label :for="`ap-port-${app.id}`">{{ t("appCenter.port") }}</Label>
              <Input :id="`ap-port-${app.id}`" v-model.number="form.port" type="number" min="1024" max="65535" />
            </div>
          </div>
          <div class="space-y-1.5">
            <Label :for="`ap-pw-${app.id}`">{{ t("account.password") }}</Label>
            <Input :id="`ap-pw-${app.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="busy || !form.password || !form.pool"><LoaderCircle v-if="busy" class="animate-spin" />{{ t("appCenter.install") }}</Button>
            <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
          </div>
        </form>

        <form v-else-if="mode === `folders:${app.id}`" class="space-y-3 border-t pt-3" @submit.prevent="saveFolders(app)">
          <p class="text-xs text-muted-foreground">
            {{ t("appCenter.foldersHelp", { name: loc(app.name), path: app.share_path }) }}
            <template v-if="!app.share_write"> {{ t("appCenter.foldersReadOnly") }}</template>
          </p>
          <p class="flex items-start gap-2 rounded-lg border border-amber-500/40 bg-amber-500/5 p-2.5 text-xs">
            <TriangleAlert class="mt-0.5 size-4 shrink-0" />{{ t("appCenter.foldersExposure") }}
          </p>
          <p v-if="!shares.length" class="rounded-lg bg-muted p-3 text-sm">{{ t("appCenter.foldersNoShares") }}</p>
          <div v-for="sh in shares" :key="sh.name" class="flex flex-wrap items-center justify-between gap-2">
            <Label :for="`ap-f-${app.id}-${sh.name}`" class="font-normal">{{ sh.name }}</Label>
            <select
              :id="`ap-f-${app.id}-${sh.name}`"
              v-model="form.folders[sh.name]"
              class="h-8 rounded-md border border-input bg-transparent px-2 text-sm shadow-xs focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
            >
              <option value="none">{{ t("appCenter.access.none") }}</option>
              <option value="read">{{ t("appCenter.access.read") }}</option>
              <option v-if="app.share_write" value="write">{{ t("appCenter.access.write") }}</option>
            </select>
          </div>
          <div class="space-y-1.5">
            <Label :for="`ap-fp-${app.id}`">{{ t("account.password") }}</Label>
            <Input :id="`ap-fp-${app.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="busy || !form.password"><LoaderCircle v-if="busy" class="animate-spin" />{{ t("appCenter.save") }}</Button>
            <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
          </div>
        </form>

        <form v-else-if="mode === `remove:${app.id}`" class="space-y-3 border-t pt-3" @submit.prevent="remove(app)">
          <p class="text-sm">{{ t("appCenter.removeHelp", { name: loc(app.name) }) }}</p>
          <label class="flex items-center gap-2 text-sm"><input v-model="form.removeData" type="checkbox" class="size-4 accent-primary" />{{ t("appCenter.removeData") }}</label>
          <template v-if="form.removeData">
            <p class="rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-sm">{{ t("appCenter.removeDataHelp", { id: app.id }) }}</p>
            <div class="space-y-1.5">
              <Label :for="`ap-c-${app.id}`">{{ t("appCenter.typeId", { id: app.id }) }}</Label>
              <Input :id="`ap-c-${app.id}`" v-model="form.confirm" autocomplete="off" class="max-w-xs" />
            </div>
            <div class="space-y-1.5">
              <Label :for="`ap-rp-${app.id}`">{{ t("account.password") }}</Label>
              <Input :id="`ap-rp-${app.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
            </div>
          </template>
          <div class="flex gap-2">
            <Button type="submit" size="sm" variant="destructive" :disabled="busy || (form.removeData && (form.confirm !== app.id || !form.password))">
              <LoaderCircle v-if="busy" class="animate-spin" />{{ t("appCenter.remove") }}
            </Button>
            <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
          </div>
        </form>
      </li>
    </ul>
    <Skeleton v-if="!catalog && !error" class="h-32 rounded-xl" />
  </div>
</template>
