<script setup lang="ts">
// Control Panel (hanya admin): pintasan ke aplikasi pengaturan, layanan
// berbagi file (SMB, NFS), serta waktu dan tanggal.
import { computed, onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, Clock, LoaderCircle, Share2, TriangleAlert } from "@lucide/vue"
import { findApp, iconFor } from "@/apps/registry"
import { Button } from "@/components/ui/button"
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
const services = ref<{ smb: boolean; nfs: boolean } | null>(null)
const svcBusy = ref(false)
const svcMsg = ref("")
const svcError = ref("")
async function toggle(name: "smb" | "nfs") {
  if (!services.value) return
  svcBusy.value = true
  svcMsg.value = svcError.value = ""
  try {
    const res = await api.setServices({ [name]: !services.value[name] })
    services.value = { smb: res.smb, nfs: res.nfs }
    if (res.warning) svcError.value = t("errors.system_sync_failed")
    else svcMsg.value = t(`controlPanel.${name}${res[name] ? "On" : "Off"}`)
  } catch (e) {
    svcError.value = message(e)
  } finally {
    svcBusy.value = false
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
    Object.assign(form, { timezone: time.value.timezone, ntp: time.value.ntp })
  } catch (e) {
    timeError.value = message(e)
  }
}
const nasTime = computed(() =>
  time.value ? new Date(time.value.now).toLocaleString(locale.value, { dateStyle: "full", timeStyle: "short", timeZone: time.value.timezone || undefined }) : "",
)
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

onMounted(async () => {
  api.services().then((s) => (services.value = s), (e) => (svcError.value = message(e)))
  loadTime()
  api.timezones().then((z) => (zones.value = z.zones), () => {})
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
      <div v-for="name in ['smb', 'nfs'] as const" :key="name" class="flex items-center justify-between gap-3 border-t pt-3">
        <div>
          <p class="text-sm font-medium">{{ t(`controlPanel.${name}`) }}</p>
          <p class="text-xs text-muted-foreground">{{ t(`controlPanel.${name}Help`) }}</p>
        </div>
        <Button
          size="sm"
          role="switch"
          :aria-checked="services?.[name] ?? false"
          :variant="services?.[name] ? 'default' : 'outline'"
          :disabled="!services || svcBusy"
          @click="toggle(name)"
        >
          <LoaderCircle v-if="svcBusy" class="animate-spin" />{{ services?.[name] ? t("controlPanel.on") : t("controlPanel.off") }}
        </Button>
      </div>
      <p v-if="svcMsg" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ svcMsg }}</p>
      <p v-if="svcError" class="flex items-center gap-2 text-sm text-destructive" role="alert"><TriangleAlert class="size-4" />{{ svcError }}</p>
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
