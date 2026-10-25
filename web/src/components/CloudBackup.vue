<script setup lang="ts">
// Backup ke S3/cloud (ADR 0020): penyimpanan (wizards/cloud-repo-add.yaml,
// cloud-repo-connect.yaml), tugas (cloud-job-create.yaml), dan pemulihan ke
// shared folder baru. Password enkripsi repo baru tampil sekali dan harus
// dinyatakan sudah dicatat.
import { onMounted, onUnmounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { ArchiveRestore, CircleCheck, Cloud, Copy, KeyRound, LoaderCircle, Play, Plus, Settings2, Trash2, TriangleAlert } from "@lucide/vue"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import type { CloudJob, CloudList, CloudRepo, CloudSnapshot, PoolsResult, SharedFolder, Wizard } from "@/lib/api"
import { api, ApiError, wizardActions } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { useSession } from "@/composables/useSession"
import WizardRunner from "@/wizard/WizardRunner.vue"

const props = defineProps<{ shares: SharedFolder[]; pools: PoolsResult | null }>()
const emit = defineEmits<{ restored: [] }>()
const { t, locale } = useI18n()
const { policy } = useSession()
const data = ref<CloudList | null>(null)
const error = ref("")
const detail = ref("")
const notice = ref("")
const busy = ref("")

function show(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  error.value = t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`) : t("errors.generic")
  detail.value = e instanceof ApiError && ["cloud_failed", "cloud_denied", "conflict"].includes(e.code) ? e.message : ""
}
async function load() {
  try {
    data.value = await api.cloud()
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
const repoName = (id: string) => data.value?.repos.find((r) => r.id === id)?.name ?? id
const where = (r: CloudRepo) => `${r.endpoint}/${r.bucket}${r.prefix ? "/" + r.prefix : ""}`

// --- Wizard ---
const wizard = ref<Wizard | null>(null)
const newPassword = ref("")
const saved = ref(false)
async function openWizard(id: string) {
  error.value = detail.value = notice.value = ""
  try {
    wizard.value = await api.wizard(id)
  } catch (e) {
    show(e)
  }
}
async function submitWizard(values: Record<string, string>) {
  const res = (await wizardActions[wizard.value!.action]!(values, {})) as { password?: string }
  if (res?.password) {
    newPassword.value = res.password
    saved.value = false
  }
  return res
}
function wizardDone() {
  const id = wizard.value?.id
  wizard.value = null
  if (id === "cloud-repo-connect") notice.value = t("cloud.connected")
  else if (id === "cloud-job-create") notice.value = t("backup.created")
  load()
}
async function copyPassword() {
  try {
    await navigator.clipboard.writeText(newPassword.value)
  } catch {
    // clipboard tidak tersedia: admin menyalin manual
  }
}

// --- Tugas, repo, pemulihan ---
const mode = ref("")
const form = reactive({ hour: "", keep: "", enabled: true, password: "", snapshot: "", pool: "", name: "" })
const snaps = ref<CloudSnapshot[] | null>(null)
function start(m: string, key: string, j?: CloudJob) {
  mode.value = `${m}:${key}`
  error.value = detail.value = notice.value = ""
  Object.assign(form, { hour: String(j?.hour ?? ""), keep: String(j?.keep_days ?? ""), enabled: j?.enabled ?? true, password: "", snapshot: "", pool: props.pools?.pools[0]?.name ?? "", name: "" })
}
async function startRestore(r: CloudRepo) {
  start("restore", r.id)
  snaps.value = null
  busy.value = r.id
  try {
    snaps.value = (await api.cloudSnapshots(r.id)).reverse()
    const s = snaps.value[0]
    if (s) Object.assign(form, { snapshot: s.id, name: `${s.share || "pulih"}-cloud`.slice(0, 32) })
  } catch (e) {
    show(e)
    mode.value = ""
  } finally {
    busy.value = ""
  }
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
const saveJob = (j: CloudJob) =>
  run(
    j.id,
    () => api.setCloudJob({ id: j.id, repo: j.repo, share: j.share, hour: Number(form.hour), keep_days: Number(form.keep), enabled: form.enabled, admin_password: form.password }),
    t("backup.saved"),
  )
const runNow = (j: CloudJob) => run(j.id, () => api.runCloudJob(j.id), t("backup.started", { name: j.share }))
const removeJob = (j: CloudJob) => run(j.id, () => api.deleteCloudJob(j.id, form.password), t("backup.deleted"))
const removeRepo = (r: CloudRepo) => run(r.id, () => api.deleteCloudRepo(r.id, form.password), t("cloud.repoDeleted"))
const restore = (r: CloudRepo) =>
  run(
    r.id,
    async () => {
      await api.restoreCloud(r.id, { snapshot: form.snapshot, pool: form.pool, name: form.name.trim(), admin_password: form.password })
      emit("restored")
    },
    t("rbackup.restoreStarted", { name: form.name.trim() }),
  )
</script>

<template>
  <section class="space-y-3 border-t pt-4">
    <h3 class="flex items-center gap-2 text-sm font-semibold"><Cloud class="size-4" />{{ t("cloud.title") }}</h3>
    <WizardRunner
      v-if="wizard"
      :wizard="wizard"
      :policy="policy"
      :submit="submitWizard"
      :context="{ shares: props.shares, targets: (data?.repos ?? []).map((r) => ({ id: r.id, name: r.name, address: where(r), fingerprint: '', created: r.created })) }"
      cancelable
      @cancel="wizard = null"
      @done="wizardDone"
    />
    <template v-else>
      <p class="text-xs text-muted-foreground">{{ t("cloud.help") }}</p>
      <p v-if="data && !data.available" class="rounded-lg bg-muted p-3 text-sm">{{ t("cloud.unavailable") }}</p>

      <!-- Password enkripsi repo baru: sekali tampil -->
      <div v-if="newPassword" class="space-y-2 rounded-lg border border-amber-500/60 bg-amber-500/10 p-3 text-sm" role="alert">
        <p class="flex items-center gap-2 font-medium"><KeyRound class="size-4" />{{ t("cloud.passwordTitle") }}</p>
        <p class="font-mono text-base tracking-wide break-all select-all">{{ newPassword }}</p>
        <p class="text-xs">{{ t("cloud.passwordHelp") }}</p>
        <div class="flex flex-wrap items-center gap-3">
          <Button size="sm" variant="outline" @click="copyPassword"><Copy />{{ t("rbackup.copy") }}</Button>
          <label class="flex items-center gap-2 text-sm"><input v-model="saved" type="checkbox" class="size-4 accent-primary" />{{ t("cloud.passwordSaved") }}</label>
          <Button size="sm" :disabled="!saved" @click="newPassword = ''">{{ t("common.close") }}</Button>
        </div>
      </div>

      <p v-if="notice" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ notice }}</p>
      <div v-if="error" class="text-sm text-destructive" role="alert">
        <p class="flex items-center gap-2"><TriangleAlert class="size-4" />{{ error }}</p>
        <p v-if="detail" class="mt-1 text-xs text-muted-foreground">{{ detail }}</p>
      </div>
      <div v-if="data?.available && !newPassword" class="flex flex-wrap gap-2">
        <Button size="sm" variant="outline" @click="openWizard('cloud-repo-add')"><Plus />{{ t("cloud.addRepo") }}</Button>
        <Button size="sm" variant="ghost" @click="openWizard('cloud-repo-connect')">{{ t("cloud.connectRepo") }}</Button>
        <Button size="sm" :disabled="!data.repos.length || !props.shares.length" @click="openWizard('cloud-job-create')"><Plus />{{ t("cloud.addJob") }}</Button>
      </div>

      <!-- Penyimpanan -->
      <ul v-if="data?.repos.length" class="space-y-2">
        <li v-for="r in data.repos" :key="r.id" class="space-y-2 rounded-lg border p-2.5">
          <div class="flex flex-wrap items-center justify-between gap-2 text-sm">
            <span class="min-w-0">
              <span class="font-medium">{{ r.name }}</span>
              <span class="ml-2 font-mono text-xs text-muted-foreground">{{ where(r) }}</span>
              <Badge v-if="r.insecure_http" variant="outline" class="ml-1">HTTP</Badge>
            </span>
            <span v-if="!mode.endsWith(`:${r.id}`)" class="flex gap-1">
              <Button size="sm" variant="outline" :disabled="!!busy" @click="startRestore(r)"><LoaderCircle v-if="busy === r.id" class="animate-spin" /><ArchiveRestore v-else />{{ t("backup.restore") }}</Button>
              <Button size="sm" variant="ghost" :aria-label="t('cloud.deleteRepo')" @click="start('delrepo', r.id)"><Trash2 /></Button>
            </span>
          </div>
          <form v-if="mode === `restore:${r.id}` && snaps" class="space-y-3 border-t pt-3" @submit.prevent="restore(r)">
            <p v-if="!snaps.length" class="text-xs text-muted-foreground">{{ t("cloud.noSnapshots") }}</p>
            <template v-else>
              <p class="text-xs text-muted-foreground">{{ t("cloud.restoreHelp") }}</p>
              <div class="grid gap-3 sm:grid-cols-3">
                <div class="space-y-1.5 sm:col-span-3">
                  <Label :for="`cl-s-${r.id}`">{{ t("backup.copy") }}</Label>
                  <select
                    :id="`cl-s-${r.id}`"
                    v-model="form.snapshot"
                    class="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm shadow-xs focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
                  >
                    <option v-for="s in snaps" :key="s.id" :value="s.id">{{ when(s.time) }} · {{ s.share || s.path }} · {{ s.hostname }}</option>
                  </select>
                </div>
                <div class="space-y-1.5">
                  <Label :for="`cl-p-${r.id}`">{{ t("cloud.pool") }}</Label>
                  <select
                    :id="`cl-p-${r.id}`"
                    v-model="form.pool"
                    class="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm shadow-xs focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
                  >
                    <option v-for="p in props.pools?.pools ?? []" :key="p.name" :value="p.name">{{ p.name }}</option>
                  </select>
                </div>
                <div class="space-y-1.5 sm:col-span-2">
                  <Label :for="`cl-n-${r.id}`">{{ t("backup.newName") }}</Label>
                  <Input :id="`cl-n-${r.id}`" v-model="form.name" pattern="^[a-z][a-z0-9_-]{0,31}$" autocomplete="off" />
                </div>
              </div>
              <div class="space-y-1.5">
                <Label :for="`cl-rp-${r.id}`">{{ t("account.password") }}</Label>
                <Input :id="`cl-rp-${r.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
              </div>
            </template>
            <div class="flex gap-2">
              <Button v-if="snaps.length" type="submit" size="sm" :disabled="!!busy || !form.password || !form.snapshot || !form.pool || !form.name.trim()">{{ t("backup.restore") }}</Button>
              <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
            </div>
          </form>
          <form v-if="mode === `delrepo:${r.id}`" class="space-y-3 border-t pt-3" @submit.prevent="removeRepo(r)">
            <p class="text-xs text-muted-foreground">{{ t("cloud.deleteRepoHelp") }}</p>
            <div class="space-y-1.5">
              <Label :for="`cl-dp-${r.id}`">{{ t("account.password") }}</Label>
              <Input :id="`cl-dp-${r.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
            </div>
            <div class="flex gap-2">
              <Button type="submit" size="sm" variant="destructive" :disabled="!!busy || !form.password">{{ t("cloud.deleteRepo") }}</Button>
              <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
            </div>
          </form>
        </li>
      </ul>

      <!-- Tugas -->
      <ul class="space-y-3">
        <li v-for="j in data?.jobs ?? []" :key="j.id" class="space-y-2 rounded-xl border p-3">
          <div class="flex flex-wrap items-start justify-between gap-2">
            <div class="min-w-0 space-y-0.5">
              <p class="flex items-center gap-1.5 text-sm font-medium">
                {{ j.share }} → {{ repoName(j.repo) }}
                <Badge v-if="j.running" variant="secondary"><LoaderCircle class="size-3 animate-spin" />{{ t("backup.running") }}</Badge>
                <Badge v-else-if="!j.enabled" variant="outline">{{ t("backup.off") }}</Badge>
              </p>
              <p class="text-xs text-muted-foreground">{{ t("cloud.schedule", { hour: String(j.hour).padStart(2, "0"), days: j.keep_days }) }}</p>
              <p v-if="when(j.last_run)" class="text-xs">
                {{ t("backup.lastRun", { when: when(j.last_run) }) }}<template v-if="j.last_total_bytes"> · {{ t("cloud.sizes", { added: size(j.last_added_bytes ?? 0), total: size(j.last_total_bytes) }) }}</template>
              </p>
              <p v-else-if="!j.running" class="text-xs text-muted-foreground">{{ t("backup.never") }}</p>
              <p v-if="!j.last_ok && j.last_error" class="flex items-start gap-1 text-xs text-destructive"><TriangleAlert class="mt-0.5 size-3.5 shrink-0" />{{ t("backup.lastFailed") }}: {{ j.last_error }}</p>
            </div>
            <div v-if="!mode.endsWith(`:${j.id}`)" class="flex gap-1">
              <Button size="sm" variant="outline" :disabled="j.running || !!busy" @click="runNow(j)"><LoaderCircle v-if="busy === j.id" class="animate-spin" /><Play v-else />{{ t("backup.runNow") }}</Button>
              <Button size="sm" variant="ghost" :aria-label="t('backup.edit')" @click="start('edit', j.id, j)"><Settings2 /></Button>
              <Button size="sm" variant="ghost" :aria-label="t('backup.delete')" @click="start('delete', j.id, j)"><Trash2 /></Button>
            </div>
          </div>
          <form v-if="mode === `edit:${j.id}`" class="space-y-3 border-t pt-3" @submit.prevent="saveJob(j)">
            <div class="grid gap-3 sm:grid-cols-2">
              <div class="space-y-1.5">
                <Label :for="`cj-h-${j.id}`">{{ t("backup.hour") }}</Label>
                <Input :id="`cj-h-${j.id}`" v-model="form.hour" inputmode="numeric" autocomplete="off" />
              </div>
              <div class="space-y-1.5">
                <Label :for="`cj-k-${j.id}`">{{ t("cloud.keepDays") }}</Label>
                <Input :id="`cj-k-${j.id}`" v-model="form.keep" inputmode="numeric" autocomplete="off" />
              </div>
            </div>
            <p v-if="Number(form.keep) < j.keep_days" class="flex items-center gap-1 text-xs text-amber-700 dark:text-amber-400"><TriangleAlert class="size-3.5" />{{ t("cloud.keepWarning") }}</p>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.enabled" type="checkbox" class="size-4 accent-primary" />{{ t("backup.enabled") }}</label>
            <div class="space-y-1.5">
              <Label :for="`cj-p-${j.id}`">{{ t("account.password") }}</Label>
              <Input :id="`cj-p-${j.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
            </div>
            <div class="flex gap-2">
              <Button type="submit" size="sm" :disabled="!!busy || !form.password || !/^([01]?[0-9]|2[0-3])$/.test(form.hour) || !/^[1-9][0-9]{0,3}$/.test(form.keep)">{{ t("backup.save") }}</Button>
              <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
            </div>
          </form>
          <form v-if="mode === `delete:${j.id}`" class="space-y-3 border-t pt-3" @submit.prevent="removeJob(j)">
            <p class="text-xs text-muted-foreground">{{ t("cloud.deleteJobHelp") }}</p>
            <div class="space-y-1.5">
              <Label :for="`cj-dp-${j.id}`">{{ t("account.password") }}</Label>
              <Input :id="`cj-dp-${j.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
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
