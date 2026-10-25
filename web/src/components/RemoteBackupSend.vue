<script setup lang="ts">
// Backup ke NAS lain, sisi pengirim (ADR 0019): NAS tujuan yang dipasangkan
// (wizards/backup-target-add.yaml) dan tugas backup ke sana
// (wizards/backup-remote-create.yaml).
import { onMounted, onUnmounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, LoaderCircle, Play, Plus, Server, Settings2, Trash2, TriangleAlert } from "@lucide/vue"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import type { RemoteBackups, RemoteJob, SharedFolder, Wizard } from "@/lib/api"
import { api, ApiError, wizardActions } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { useSession } from "@/composables/useSession"
import WizardRunner from "@/wizard/WizardRunner.vue"

const props = defineProps<{ shares: SharedFolder[] }>()
const { t, locale } = useI18n()
const { policy } = useSession()
const data = ref<RemoteBackups | null>(null)
const error = ref("")
const detail = ref("")
const notice = ref("")
const busy = ref("")

function show(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  error.value = t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`) : t("errors.generic")
  detail.value = e instanceof ApiError && ["pairing_failed", "conflict", "backup_diverged"].includes(e.code) ? e.message : ""
}
async function load() {
  try {
    data.value = await api.remoteBackups()
  } catch (e) {
    show(e)
  }
}
let timer: number | undefined
onMounted(() => {
  load()
  timer = window.setInterval(() => {
    if (data.value?.jobs.some((j) => j.running)) load()
  }, 5000)
})
onUnmounted(() => window.clearInterval(timer))

const size = (b: number) => formatBytes(b, locale.value, 1000)
const when = (s?: string) => (s && !s.startsWith("0001") ? new Date(s).toLocaleString(locale.value, { dateStyle: "medium", timeStyle: "short" }) : "")
// Sidik jari dipendekkan untuk dibandingkan dengan yang tampil di NAS tujuan.
const shortFp = (fp: string) => fp.slice(0, 32).match(/.{4}/g)?.join(" ") ?? fp

const wizard = ref<Wizard | null>(null)
async function openWizard(id: string) {
  error.value = detail.value = notice.value = ""
  try {
    wizard.value = await api.wizard(id)
  } catch (e) {
    show(e)
  }
}
const submitWizard = (values: Record<string, string>) => wizardActions[wizard.value!.action]!(values, {})
function wizardDone() {
  notice.value = wizard.value?.id === "backup-target-add" ? t("rbackup.paired") : t("backup.created")
  wizard.value = null
  load()
}

const mode = ref("")
const form = reactive({ hour: "", enabled: true, password: "" })
function start(m: string, j: RemoteJob) {
  mode.value = `${m}:${j.id}`
  error.value = detail.value = notice.value = ""
  Object.assign(form, { hour: String(j.hour), enabled: j.enabled, password: "" })
}
async function run(key: string, fn: () => Promise<unknown>, done: string) {
  busy.value = key
  error.value = detail.value = notice.value = ""
  try {
    await fn()
    notice.value = done
    mode.value = ""
    await load()
  } catch (e) {
    show(e)
  } finally {
    busy.value = ""
  }
}
const save = (j: RemoteJob) =>
  run(j.id, () => api.setRemoteJob({ id: j.id, share: j.share, target: j.target, hour: Number(form.hour), enabled: form.enabled, admin_password: form.password }), t("backup.saved"))
const runNow = (j: RemoteJob) => run(j.id, () => api.runRemoteJob(j.id), t("backup.started", { name: j.share }))
const remove = (j: RemoteJob) => run(j.id, () => api.deleteRemoteJob(j.id, form.password), t("backup.deleted"))
const removeTarget = (id: string) => run(id, () => api.deleteTarget(id), t("rbackup.targetDeleted"))
</script>

<template>
  <section class="space-y-3 border-t pt-4">
    <h3 class="flex items-center gap-2 text-sm font-semibold"><Server class="size-4" />{{ t("rbackup.sendTitle") }}</h3>
    <WizardRunner
      v-if="wizard"
      :wizard="wizard"
      :policy="policy"
      :submit="submitWizard"
      :context="{ shares: props.shares, targets: data?.targets ?? [] }"
      cancelable
      @cancel="wizard = null"
      @done="wizardDone"
    />
    <template v-else>
      <p class="text-xs text-muted-foreground">{{ t("rbackup.sendHelp") }}</p>
      <p v-if="notice" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ notice }}</p>
      <div v-if="error" class="text-sm text-destructive" role="alert">
        <p class="flex items-center gap-2"><TriangleAlert class="size-4" />{{ error }}</p>
        <p v-if="detail" class="mt-1 text-xs text-muted-foreground">{{ detail }}</p>
      </div>
      <div class="flex flex-wrap gap-2">
        <Button size="sm" variant="outline" @click="openWizard('backup-target-add')"><Plus />{{ t("rbackup.addTarget") }}</Button>
        <Button size="sm" :disabled="!data?.targets.length || !props.shares.length" @click="openWizard('backup-remote-create')"><Plus />{{ t("rbackup.addJob") }}</Button>
      </div>

      <ul v-if="data?.targets.length" class="space-y-1 text-sm">
        <li v-for="x in data.targets" :key="x.id" class="flex flex-wrap items-center justify-between gap-2 rounded-lg border px-3 py-2">
          <span class="min-w-0">
            <span class="font-medium">{{ x.name }}</span>
            <span class="ml-2 font-mono text-xs text-muted-foreground">{{ x.address }}<template v-if="x.remote_host"> · {{ x.remote_host }}</template></span>
            <span class="block font-mono text-[11px] text-muted-foreground">{{ t("rbackup.fingerprint") }}: {{ shortFp(x.fingerprint) }}</span>
          </span>
          <Button size="sm" variant="ghost" :aria-label="t('rbackup.deleteTarget')" :disabled="!!busy" @click="removeTarget(x.id)"><Trash2 /></Button>
        </li>
      </ul>

      <ul class="space-y-3">
        <li v-for="j in data?.jobs ?? []" :key="j.id" class="space-y-2 rounded-xl border p-3">
          <div class="flex flex-wrap items-start justify-between gap-2">
            <div class="min-w-0 space-y-0.5">
              <p class="flex items-center gap-1.5 text-sm font-medium">
                {{ j.share }} → {{ j.target_name }}
                <Badge v-if="j.running" variant="secondary"><LoaderCircle class="size-3 animate-spin" />{{ t("backup.running") }}</Badge>
                <Badge v-else-if="!j.enabled" variant="outline">{{ t("backup.off") }}</Badge>
              </p>
              <p class="text-xs text-muted-foreground">
                {{ t("rbackup.schedule", { hour: String(j.hour).padStart(2, "0") }) }}<template v-if="j.last_copies"> · {{ t("rbackup.remoteCopies", { n: j.last_copies }) }}</template>
              </p>
              <p v-if="when(j.last_run)" class="text-xs">{{ t("backup.lastRun", { when: when(j.last_run) }) }}<template v-if="j.last_bytes"> · {{ t("backup.sent", { size: size(j.last_bytes) }) }}</template></p>
              <p v-else-if="!j.running" class="text-xs text-muted-foreground">{{ t("backup.never") }}</p>
              <p v-if="!j.last_ok && j.last_error" class="flex items-start gap-1 text-xs text-destructive"><TriangleAlert class="mt-0.5 size-3.5 shrink-0" />{{ t("backup.lastFailed") }}: {{ j.last_error }}</p>
            </div>
            <div v-if="!mode.endsWith(`:${j.id}`)" class="flex gap-1">
              <Button size="sm" variant="outline" :disabled="j.running || !!busy" @click="runNow(j)"><LoaderCircle v-if="busy === j.id" class="animate-spin" /><Play v-else />{{ t("backup.runNow") }}</Button>
              <Button size="sm" variant="ghost" :aria-label="t('backup.edit')" @click="start('edit', j)"><Settings2 /></Button>
              <Button size="sm" variant="ghost" :aria-label="t('backup.delete')" @click="start('delete', j)"><Trash2 /></Button>
            </div>
          </div>
          <form v-if="mode === `edit:${j.id}`" class="space-y-3 border-t pt-3" @submit.prevent="save(j)">
            <div class="space-y-1.5">
              <Label :for="`rb-h-${j.id}`">{{ t("backup.hour") }}</Label>
              <Input :id="`rb-h-${j.id}`" v-model="form.hour" inputmode="numeric" autocomplete="off" class="w-20" />
            </div>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.enabled" type="checkbox" class="size-4 accent-primary" />{{ t("backup.enabled") }}</label>
            <div class="space-y-1.5">
              <Label :for="`rb-p-${j.id}`">{{ t("account.password") }}</Label>
              <Input :id="`rb-p-${j.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
            </div>
            <div class="flex gap-2">
              <Button type="submit" size="sm" :disabled="!!busy || !form.password || !/^([01]?[0-9]|2[0-3])$/.test(form.hour)">{{ t("backup.save") }}</Button>
              <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
            </div>
          </form>
          <form v-if="mode === `delete:${j.id}`" class="space-y-3 border-t pt-3" @submit.prevent="remove(j)">
            <p class="text-xs text-muted-foreground">{{ t("rbackup.deleteJobHelp", { target: j.target_name }) }}</p>
            <div class="space-y-1.5">
              <Label :for="`rb-dp-${j.id}`">{{ t("account.password") }}</Label>
              <Input :id="`rb-dp-${j.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
            </div>
            <div class="flex gap-2">
              <Button type="submit" size="sm" variant="destructive" :disabled="!!busy || !form.password">{{ t("backup.delete") }}</Button>
              <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
            </div>
          </form>
        </li>
      </ul>
    </template>
  </section>
</template>
