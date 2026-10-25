<script setup lang="ts">
// Control Panel (hanya admin): pintasan ke aplikasi pengaturan, layanan
// berbagi file (SMB, NFS), serta waktu dan tanggal.
import { computed, onMounted, onUnmounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, Clock, LoaderCircle, PackageCheck, Share2, TriangleAlert } from "@lucide/vue"
import { findApp, iconFor } from "@/apps/registry"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useLauncher } from "@/composables/useLauncher"
import { api, ApiError } from "@/lib/api"

const { t, locale } = useI18n()
const { launch } = useLauncher()

// Pintasan ke aplikasi pengaturan yang sudah ada (data, bukan kode).
const shortcuts = ["network", "users", "shared-folders", "storage", "account-security", "resource-monitor"]
  .map((id) => findApp(id))
  .filter((a) => a?.status === "ready")

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`, { message: e instanceof Error ? e.message : "" }) : t("errors.generic")
}

// --- Layanan ---
type Service = "smb" | "nfs" | "webdav"
const services = ref<Record<Service, boolean> | null>(null)
const svcBusy = ref<"" | Service>("")
const svcMsg = ref("")
const svcError = ref("")
// Mematikan layanan memutus semua klien: harus dikonfirmasi dulu.
const confirmOff = ref<"" | Service>("")
async function toggle(name: Service) {
  if (!services.value) return
  if (services.value[name] && confirmOff.value !== name) {
    confirmOff.value = name
    return
  }
  confirmOff.value = ""
  svcBusy.value = name
  svcMsg.value = svcError.value = ""
  try {
    const res = await api.setServices({ [name]: !services.value[name] })
    services.value = { smb: res.smb, nfs: res.nfs, webdav: res.webdav }
    if (res.warning) svcError.value = t("errors.system_sync_failed")
    else svcMsg.value = t(`controlPanel.${name}${res[name] ? "On" : "Off"}`)
  } catch (e) {
    svcError.value = message(e)
  } finally {
    svcBusy.value = ""
  }
}

// --- Waktu ---
const time = ref<{ timezone: string; now: string; ntp: boolean; synchronized: boolean } | null>(null)
const zones = ref<string[]>([])
const form = reactive({ timezone: "", ntp: true })
const timeBusy = ref(false)
const timeMsg = ref("")
const timeError = ref("")
async function loadTime() {
  try {
    time.value = await api.timeStatus()
    fetchedAt.value = tick.value = Date.now()
    Object.assign(form, { timezone: time.value.timezone, ntp: time.value.ntp })
  } catch (e) {
    timeError.value = message(e)
  }
}
// Jam NAS berjalan dari waktu server (diperbarui setiap 30 detik).
const fetchedAt = ref(Date.now())
const tick = ref(Date.now())
let clock: number | undefined
onUnmounted(() => window.clearInterval(clock))
const nasTime = computed(() => {
  if (!time.value) return ""
  const at = new Date(Date.parse(time.value.now) + (tick.value - fetchedAt.value))
  try {
    return at.toLocaleString(locale.value, { dateStyle: "full", timeStyle: "short", timeZone: time.value.timezone || undefined })
  } catch {
    // zona waktu yang tidak dikenal browser
    return `${at.toLocaleString(locale.value, { dateStyle: "full", timeStyle: "short", timeZone: "UTC" })} UTC`
  }
})
const timeChanged = computed(() => !!time.value && (form.timezone !== time.value.timezone || form.ntp !== time.value.ntp))
async function saveTime() {
  timeBusy.value = true
  timeMsg.value = timeError.value = ""
  try {
    await api.setTime(form.timezone, form.ntp)
    timeMsg.value = t("controlPanel.timeSaved")
    await loadTime()
  } catch (e) {
    timeError.value = message(e)
  } finally {
    timeBusy.value = false
  }
}

// --- Pembaruan sistem ---
type Pkg = { name: string; from?: string; to: string }
type UpdStatus = Awaited<ReturnType<typeof api.updatesStatus>>
const upd = ref<UpdStatus | null>(null)
const pkgs = ref<Pkg[] | null>(null)
const checking = ref(false)
const updError = ref("")
const updPassword = ref("")
const installing = ref(false)
let updTimer: number | undefined
onUnmounted(() => window.clearTimeout(updTimer))
async function pollUpdates() {
  try {
    upd.value = await api.updatesStatus()
  } catch {
    // Panel mungkin sedang dinyalakan ulang (paket Sapo diperbarui): coba lagi.
  }
  // Selesai: unit tidak berjalan lagi dan hasilnya sudah ditulis (atau
  // terputus tanpa hasil akhir).
  if (installing.value && upd.value && !upd.value.running && (upd.value.last?.finished_at || upd.value.last?.interrupted)) installing.value = false
  if (installing.value || upd.value?.running) updTimer = window.setTimeout(pollUpdates, 3000)
}
async function check() {
  checking.value = true
  updError.value = ""
  try {
    pkgs.value = (await api.checkUpdates()).packages
  } catch (e) {
    updError.value = message(e)
  } finally {
    checking.value = false
  }
}
const sapoUpdate = computed(() => pkgs.value?.some((p) => p.name === "sapo"))
async function install() {
  updError.value = ""
  try {
    await api.installUpdates(updPassword.value)
    updPassword.value = ""
    pkgs.value = null
    installing.value = true
    pollUpdates()
  } catch (e) {
    updError.value = message(e)
  }
}

onMounted(async () => {
  pollUpdates()
  api.services().then((s) => (services.value = s), (e) => (svcError.value = message(e)))
  loadTime()
  clock = window.setInterval(() => (tick.value = Date.now()), 30_000)
  api.timezones().then(
    (z) => (zones.value = z.zones),
    (e) => (timeError.value = message(e)),
  )
})
</script>

<template>
  <div class="space-y-6 p-4">
    <!-- Pintasan -->
    <section class="space-y-2">
      <h2 class="text-sm font-medium">{{ t("controlPanel.settings") }}</h2>
      <div class="grid gap-2 sm:grid-cols-2 md:grid-cols-3">
        <button
          v-for="a in shortcuts"
          :key="a!.id"
          type="button"
          class="flex items-start gap-3 rounded-xl border p-3 text-left hover:bg-accent"
          @click="launch(a!.id)"
        >
          <component :is="iconFor(a!.icon)" class="mt-0.5 size-5 shrink-0 text-muted-foreground" />
          <span class="min-w-0">
            <span class="block text-sm font-medium">{{ t(`apps.${a!.id}.title`) }}</span>
            <span class="block text-xs text-muted-foreground">{{ t(`apps.${a!.id}.description`) }}</span>
          </span>
        </button>
      </div>
    </section>

    <!-- Layanan berbagi file -->
    <section class="space-y-3 rounded-xl border p-4">
      <h2 class="flex items-center gap-2 text-sm font-medium"><Share2 class="size-4 text-muted-foreground" />{{ t("controlPanel.services") }}</h2>
      <p class="text-xs text-muted-foreground">{{ t("controlPanel.servicesHelp") }}</p>
      <div v-for="name in ['smb', 'nfs', 'webdav'] as const" :key="name" class="flex items-center justify-between gap-3 border-t pt-3">
        <div>
          <p class="text-sm font-medium">{{ t(`controlPanel.${name}`) }}</p>
          <p class="text-xs text-muted-foreground">{{ t(`controlPanel.${name}Help`) }}</p>
        </div>
        <Button
          size="sm"
          role="switch"
          :aria-checked="services?.[name] ?? false"
          :variant="services?.[name] ? 'default' : 'outline'"
          :disabled="!services || !!svcBusy"
          @click="toggle(name)"
        >
          <LoaderCircle v-if="svcBusy === name" class="animate-spin" />{{ services?.[name] ? t("controlPanel.on") : t("controlPanel.off") }}
        </Button>
      </div>
      <div v-if="confirmOff" class="flex flex-wrap items-center gap-2 rounded-lg border border-amber-500/40 bg-amber-500/5 p-2.5 text-sm">
        <span class="flex-1">{{ t("controlPanel.confirmOff", { name: t(`controlPanel.${confirmOff}`) }) }}</span>
        <Button size="sm" variant="destructive" @click="toggle(confirmOff)">{{ t("controlPanel.turnOff") }}</Button>
        <Button size="sm" variant="ghost" @click="confirmOff = ''">{{ t("common.cancel") }}</Button>
      </div>
      <p v-if="svcMsg" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ svcMsg }}</p>
      <p v-if="svcError" class="flex items-center gap-2 text-sm text-destructive" role="alert"><TriangleAlert class="size-4" />{{ svcError }}</p>
    </section>

    <!-- Pembaruan sistem -->
    <section class="space-y-3 rounded-xl border p-4">
      <h2 class="flex items-center gap-2 text-sm font-medium"><PackageCheck class="size-4 text-muted-foreground" />{{ t("controlPanel.updates") }}</h2>
      <p class="text-xs text-muted-foreground">{{ t("controlPanel.updatesHelp") }}</p>
      <div v-if="upd?.running || installing" class="space-y-2">
        <p class="flex items-center gap-2 text-sm"><LoaderCircle class="size-4 animate-spin" />{{ t("controlPanel.installing") }}</p>
      </div>
      <p v-else-if="upd?.last?.interrupted" class="flex items-center gap-2 text-sm text-destructive">
        <TriangleAlert class="size-4" />{{ t("controlPanel.installInterrupted") }}
      </p>
      <p v-else-if="upd?.last?.finished_at" :class="['flex items-center gap-2 text-sm', upd.last.ok ? '' : 'text-destructive']">
        <CircleCheck v-if="upd.last.ok" class="size-4 text-emerald-600" /><TriangleAlert v-else class="size-4" />
        {{ upd.last.ok ? t("controlPanel.installed", { when: new Date(upd.last.finished_at).toLocaleString(locale) }) : t("controlPanel.installFailed", { code: upd.last.exit_code }) }}
      </p>
      <details v-if="upd?.log.length" class="text-xs" :open="installing || upd?.running">
        <summary class="cursor-pointer text-muted-foreground">{{ t("controlPanel.updateLog") }}</summary>
        <pre class="mt-1 max-h-48 overflow-auto rounded-lg bg-muted p-2 font-mono whitespace-pre-wrap">{{ upd.log.join("\n") }}</pre>
      </details>
      <template v-if="!upd?.running && !installing">
        <Button size="sm" variant="outline" :disabled="checking" @click="check"><LoaderCircle v-if="checking" class="animate-spin" />{{ t("controlPanel.checkUpdates") }}</Button>
        <p v-if="pkgs && !pkgs.length" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ t("controlPanel.upToDate") }}</p>
        <form v-else-if="pkgs" class="space-y-3" @submit.prevent="install">
          <p class="text-sm">{{ t("controlPanel.available", { count: pkgs.length }) }}</p>
          <ul class="max-h-40 overflow-auto rounded-lg bg-muted p-2 font-mono text-xs">
            <li v-for="p in pkgs" :key="p.name">{{ p.name }} {{ p.from ? `${p.from} → ` : "" }}{{ p.to }}</li>
          </ul>
          <p v-if="sapoUpdate" class="rounded-lg border border-amber-500/40 bg-amber-500/5 p-2.5 text-xs">{{ t("controlPanel.sapoUpdate") }}</p>
          <div class="space-y-1.5">
            <Label for="cp-upd-pw">{{ t("account.password") }}</Label>
            <Input id="cp-upd-pw" v-model="updPassword" type="password" autocomplete="current-password" class="max-w-xs" />
          </div>
          <Button type="submit" size="sm" :disabled="!updPassword">{{ t("controlPanel.install") }}</Button>
        </form>
      </template>
      <p v-if="updError" class="flex items-center gap-2 text-sm text-destructive" role="alert"><TriangleAlert class="size-4" />{{ updError }}</p>
    </section>

    <!-- Waktu dan tanggal -->
    <section class="space-y-3 rounded-xl border p-4">
      <h2 class="flex items-center gap-2 text-sm font-medium"><Clock class="size-4 text-muted-foreground" />{{ t("controlPanel.time") }}</h2>
      <template v-if="time">
        <p class="text-sm">{{ nasTime }}</p>
        <p class="text-xs text-muted-foreground">
          {{ time.ntp ? (time.synchronized ? t("controlPanel.ntpSynced") : t("controlPanel.ntpWaiting")) : t("controlPanel.ntpOff") }}
        </p>
        <form class="space-y-3" @submit.prevent="saveTime">
          <div class="space-y-1.5">
            <Label for="cp-tz">{{ t("controlPanel.timezone") }}</Label>
            <select
              id="cp-tz"
              v-model="form.timezone"
              class="h-9 w-full max-w-sm rounded-md border border-input bg-transparent px-3 text-sm shadow-xs focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
            >
              <option v-if="!zones.includes(form.timezone)" :value="form.timezone">{{ form.timezone }}</option>
              <option v-for="z in zones" :key="z" :value="z">{{ z }}</option>
            </select>
          </div>
          <label class="flex items-center gap-2 text-sm">
            <input v-model="form.ntp" type="checkbox" class="size-4 accent-primary" />{{ t("controlPanel.ntp") }}
          </label>
          <Button type="submit" size="sm" :disabled="timeBusy || !timeChanged"><LoaderCircle v-if="timeBusy" class="animate-spin" />{{ t("users.save") }}</Button>
        </form>
      </template>
      <p v-if="timeMsg" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ timeMsg }}</p>
      <p v-if="timeError" class="flex items-center gap-2 text-sm text-destructive" role="alert"><TriangleAlert class="size-4" />{{ timeError }}</p>
    </section>
  </div>
</template>
