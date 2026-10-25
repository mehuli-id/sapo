<script setup lang="ts">
// Notifikasi email (ADR 0018) di Control Panel: server diatur lewat wizard
// (wizards/notify-setup.yaml); di sini status, email uji, jenis kejadian,
// ambang pool penuh, dan nyala/mati.
import { computed, onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { BellRing, CircleCheck, LoaderCircle, Send, TriangleAlert } from "@lucide/vue"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import type { NotifyInfo, Wizard } from "@/lib/api"
import { api, ApiError, wizardActions } from "@/lib/api"
import { useSession } from "@/composables/useSession"
import WizardRunner from "@/wizard/WizardRunner.vue"

const { t, locale } = useI18n()
const { policy } = useSession()
const info = ref<NotifyInfo | null>(null)
const error = ref("")
const detail = ref("")
const notice = ref("")
const busy = ref("")

function show(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  error.value = t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`, { max: info.value?.max_recipients ?? 5 }) : t("errors.generic")
  // Jawaban server SMTP membantu admin memperbaiki pengaturan.
  detail.value = e instanceof ApiError && ["notify_failed", "smtp_invalid"].includes(e.code) ? e.message : ""
}
async function load() {
  try {
    info.value = await api.notify()
    const s = info.value.settings
    Object.assign(form, {
      enabled: s.enabled,
      capacity: String(s.capacity_pct || info.value.default_capacity_pct),
      events: Object.fromEntries(info.value.events.map((e) => [e, !(s.events_off ?? []).includes(e)])),
      password: "",
    })
  } catch (e) {
    show(e)
  }
}
onMounted(load)

// Waktu Go yang kosong ("0001-01-01…") = belum pernah.
const when = (s?: string) => (s && !s.startsWith("0001") ? new Date(s).toLocaleString(locale.value, { dateStyle: "medium", timeStyle: "short" }) : "")
const configured = computed(() => !!info.value?.settings.host)

// --- Wizard ---
const wizard = ref<Wizard | null>(null)
async function openWizard() {
  error.value = detail.value = notice.value = ""
  try {
    wizard.value = await api.wizard("notify-setup")
  } catch (e) {
    show(e)
  }
}
const submitWizard = (values: Record<string, string>) => wizardActions[wizard.value!.action]!(values, {})
function wizardDone() {
  wizard.value = null
  notice.value = t("notify.saved")
  load()
}

// --- Pengaturan ---
const form = reactive({ enabled: true, capacity: "90", events: {} as Record<string, boolean>, password: "" })
const capacityOK = computed(() => /^\d{1,2}$/.test(form.capacity) && Number(form.capacity) >= 1)
async function run(key: string, fn: () => Promise<unknown>, done: string) {
  busy.value = key
  error.value = detail.value = notice.value = ""
  try {
    await fn()
    notice.value = done
    await load()
  } catch (e) {
    show(e)
  } finally {
    busy.value = ""
  }
}
const save = () => {
  const s = info.value!.settings
  return run(
    "save",
    () =>
      api.setNotify({
        ...s,
        enabled: form.enabled,
        capacity_pct: Number(form.capacity),
        events_off: info.value!.events.filter((e) => !form.events[e]),
        password: "",
        admin_password: form.password,
      }),
    t("notify.saved"),
  )
}
const test = () => run("test", () => api.testNotify(), t("notify.testSent", { to: info.value!.settings.to.join(", ") }))
</script>

<template>
  <section class="space-y-3 rounded-xl border p-4">
    <h2 class="flex items-center gap-2 text-sm font-medium"><BellRing class="size-4 text-muted-foreground" />{{ t("notify.title") }}</h2>
    <WizardRunner v-if="wizard" :wizard="wizard" :policy="policy" :submit="submitWizard" :context="{}" cancelable @cancel="wizard = null" @done="wizardDone" />
    <template v-else>
      <p class="text-xs text-muted-foreground">{{ t("notify.help") }}</p>
      <p v-if="notice" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ notice }}</p>
      <div v-if="error" class="text-sm text-destructive" role="alert">
        <p class="flex items-center gap-2"><TriangleAlert class="size-4" />{{ error }}</p>
        <p v-if="detail" class="mt-1 text-xs text-muted-foreground">{{ detail }}</p>
      </div>

      <Button v-if="info && !configured" size="sm" @click="openWizard">{{ t("notify.setup") }}</Button>

      <template v-if="info && configured">
        <div class="space-y-1 text-sm">
          <p class="flex flex-wrap items-center gap-2">
            <Badge :variant="info.settings.enabled ? 'secondary' : 'outline'">{{ info.settings.enabled ? t("controlPanel.on") : t("controlPanel.off") }}</Badge>
            <span class="font-mono text-xs">{{ info.settings.host }}:{{ info.settings.port }}</span>
            <span class="text-xs text-muted-foreground">{{ t(`notify.security.${info.settings.security}`) }}</span>
          </p>
          <p class="text-xs">{{ t("notify.to", { to: info.settings.to.join(", ") }) }}</p>
          <p v-if="when(info.last_sent)" class="text-xs text-muted-foreground">{{ t("notify.lastSent", { when: when(info.last_sent) }) }}</p>
          <p v-if="info.last_error" class="flex items-start gap-1 text-xs text-destructive">
            <TriangleAlert class="mt-0.5 size-3.5 shrink-0" />{{ t("notify.lastError", { when: when(info.last_error_at) }) }}: {{ info.last_error }}
          </p>
        </div>
        <div v-if="info.alerts.length" class="space-y-1 rounded-lg border border-amber-500/40 bg-amber-500/5 p-2.5 text-xs">
          <p class="font-medium">{{ t("notify.active") }}</p>
          <p v-for="a in info.alerts" :key="a.event + a.name">{{ t(`notify.events.${a.event}`) }}: {{ a.name }} ({{ a.detail }})</p>
        </div>
        <div class="flex flex-wrap gap-2">
          <Button size="sm" variant="outline" :disabled="!!busy" @click="test"><LoaderCircle v-if="busy === 'test'" class="animate-spin" /><Send v-else />{{ t("notify.test") }}</Button>
          <Button size="sm" variant="ghost" @click="openWizard">{{ t("notify.change") }}</Button>
        </div>

        <form class="space-y-3 border-t pt-3" @submit.prevent="save">
          <label class="flex items-center gap-2 text-sm"><input v-model="form.enabled" type="checkbox" class="size-4 accent-primary" />{{ t("notify.enabled") }}</label>
          <fieldset class="space-y-1.5">
            <legend class="text-sm font-medium">{{ t("notify.eventsTitle") }}</legend>
            <label v-for="e in info.events" :key="e" class="flex items-center gap-2 text-sm">
              <input v-model="form.events[e]" type="checkbox" class="size-4 accent-primary" />{{ t(`notify.events.${e}`) }}
            </label>
          </fieldset>
          <div class="space-y-1.5">
            <Label for="nt-cap">{{ t("notify.capacity") }}</Label>
            <Input id="nt-cap" v-model="form.capacity" inputmode="numeric" autocomplete="off" class="max-w-24" />
          </div>
          <div class="space-y-1.5">
            <Label for="nt-pw">{{ t("account.password") }}</Label>
            <Input id="nt-pw" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
          </div>
          <Button type="submit" size="sm" :disabled="!!busy || !form.password || !capacityOK"><LoaderCircle v-if="busy === 'save'" class="animate-spin" />{{ t("users.save") }}</Button>
        </form>
      </template>
    </template>
  </section>
</template>
