<script setup lang="ts">
// Backup (ADR 0017): tugas backup shared folder ke pool lain. Tugas dibuat
// lewat wizard (wizards/backup-create.yaml); di sini jadwal diubah, backup
// dijalankan sekarang, salinan dipulihkan sebagai folder baru, dan tugas
// dihapus (opsional beserta salinannya).
import { computed, onMounted, onUnmounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { ArchiveRestore, CircleCheck, DatabaseBackup, LoaderCircle, Play, Plus, Settings2, Trash2, TriangleAlert } from "@lucide/vue"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import type { BackupStatus, BackupsList, PoolsResult, SharedFolder, Wizard } from "@/lib/api"
import { api, ApiError, wizardActions } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { useSession } from "@/composables/useSession"
import WizardRunner from "@/wizard/WizardRunner.vue"

const { t, locale } = useI18n()
const { policy } = useSession()
const data = ref<BackupsList | null>(null)
const pools = ref<PoolsResult | null>(null)
const shares = ref<SharedFolder[]>([])
const error = ref("")
const detail = ref("")
const notice = ref("")
const busy = ref("")

function show(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  error.value = t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`) : t("errors.generic")
  // Pesan sapod (misalnya alasan bentrok) sebagai keterangan tambahan.
  detail.value = e instanceof ApiError && ["conflict", "backup_diverged", "not_found"].includes(e.code) ? e.message : ""
}
async function load() {
  try {
    data.value = await api.backups()
  } catch (e) {
    show(e)
  }
}
let timer: number | undefined
onMounted(async () => {
  await load()
  try {
    ;[pools.value, shares.value] = await Promise.all([api.pools(), api.shares()])
  } catch (e) {
    show(e)
  }
  // Diperbarui berkala selama ada backup atau pemulihan yang berjalan.
  timer = window.setInterval(() => {
    if (data.value?.jobs.some((j) => j.running) || data.value?.restores.some((r) => r.running)) load()
  }, 5000)
})
onUnmounted(() => window.clearInterval(timer))

const size = (b: number) => formatBytes(b, locale.value, 1000)
const when = (s?: string) => (s ? new Date(s).toLocaleString(locale.value, { dateStyle: "medium", timeStyle: "short" }) : "")
const restores = computed(() => [...(data.value?.restores ?? [])].reverse().slice(0, 5))

// --- Wizard tugas baru ---
const wizard = ref<Wizard | null>(null)
async function openWizard() {
  error.value = notice.value = ""
  try {
    wizard.value = await api.wizard("backup-create")
  } catch (e) {
    show(e)
  }
}
const submitWizard = (values: Record<string, string>) => wizardActions[wizard.value!.action]!(values, {})
function wizardDone() {
  wizard.value = null
  notice.value = t("backup.created")
  load()
}

// --- Ubah, hapus, pulihkan ---
const mode = ref("") // "edit:<id>" | "delete:<id>" | "restore:<id>"
const form = reactive({ hour: "", keep: "", enabled: true, deleteData: false, confirm: "", copy: "", name: "", password: "" })
function start(m: string, j: BackupStatus) {
  mode.value = `${m}:${j.id}`
  error.value = detail.value = notice.value = ""
  const latest = j.copies.at(-1)?.name ?? ""
  Object.assign(form, { hour: String(j.hour), keep: String(j.keep), enabled: j.enabled, deleteData: false, confirm: "", copy: latest, name: `${j.share}-pulih`.slice(0, 32), password: "" })
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
const save = (j: BackupStatus) =>
  run(j.id, () => api.setBackup({ id: j.id, share: j.share, dest_pool: j.dest_pool, hour: Number(form.hour), keep: Number(form.keep), enabled: form.enabled, admin_password: form.password }), t("backup.saved"))
const runNow = (j: BackupStatus) => run(j.id, () => api.runBackup(j.id), t("backup.started", { name: j.share }))
const remove = (j: BackupStatus) =>
  run(j.id, () => api.deleteBackup(j.id, { delete_data: form.deleteData, confirm_name: form.confirm, admin_password: form.password }), t("backup.deleted"))
const restore = (j: BackupStatus) =>
  run(j.id, () => api.restoreBackup(j.id, { copy: form.copy, name: form.name.trim(), admin_password: form.password }), t("backup.restoreStarted", { name: form.name.trim() }))
const hourOK = computed(() => /^([01]?[0-9]|2[0-3])$/.test(form.hour))
const keepOK = computed(() => /^\d{1,3}$/.test(form.keep) && Number(form.keep) >= 1 && Number(form.keep) <= 366)
</script>

<template>
  <div v-if="wizard" class="p-4">
    <WizardRunner :wizard="wizard" :policy="policy" :submit="submitWizard" :context="{ pools: pools ?? undefined, shares }" cancelable @cancel="wizard = null" @done="wizardDone" />
  </div>

  <div v-else class="space-y-4 p-4">
    <div class="flex flex-wrap items-start justify-between gap-2">
      <p class="max-w-prose text-sm text-muted-foreground">{{ t("backup.help") }}</p>
      <Button size="sm" :disabled="!shares.length || (pools?.pools.length ?? 0) < 2" @click="openWizard"><Plus />{{ t("backup.create") }}</Button>
    </div>
    <p v-if="pools && pools.pools.length < 2" class="rounded-lg bg-muted p-3 text-sm">{{ t("backup.needTwoPools") }}</p>
    <p v-if="notice" class="flex items-center gap-2 rounded-lg border border-emerald-500/40 bg-emerald-500/5 p-2.5 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ notice }}</p>
    <div v-if="error" class="rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-sm" role="alert">
      <p class="flex items-center gap-2"><TriangleAlert class="size-4" />{{ error }}</p>
      <p v-if="detail" class="mt-1 text-xs text-muted-foreground">{{ detail }}</p>
    </div>
    <p v-if="data && !data.jobs.length" class="rounded-lg bg-muted p-3 text-sm">{{ t("backup.none") }}</p>

    <ul class="space-y-3">
      <li v-for="j in data?.jobs ?? []" :key="j.id" class="space-y-2 rounded-xl border p-3">
        <div class="flex flex-wrap items-start justify-between gap-2">
          <div class="min-w-0 space-y-0.5">
            <p class="flex items-center gap-1.5 text-sm font-medium">
              <DatabaseBackup class="size-4" />{{ j.share }} → {{ j.dest_pool }}
              <Badge v-if="j.running" variant="secondary"><LoaderCircle class="size-3 animate-spin" />{{ t("backup.running") }}</Badge>
              <Badge v-else-if="!j.enabled" variant="outline">{{ t("backup.off") }}</Badge>
            </p>
            <p class="text-xs text-muted-foreground">
              {{ t("backup.schedule", { hour: String(j.hour).padStart(2, "0"), keep: j.keep }) }}
              · {{ t("backup.copies", { n: j.copies.length }) }}<template v-if="j.dest_exists"> · {{ size(j.used_bytes) }}</template>
            </p>
            <p v-if="j.last_run" class="text-xs">{{ t("backup.lastRun", { when: when(j.last_run) }) }}<template v-if="j.last_bytes"> · {{ t("backup.sent", { size: size(j.last_bytes) }) }}</template></p>
            <p v-else-if="!j.running" class="text-xs text-muted-foreground">{{ t("backup.never") }}</p>
            <p v-if="!j.last_ok && j.last_error" class="flex items-center gap-1 text-xs text-destructive"><TriangleAlert class="size-3.5" />{{ t("backup.lastFailed") }}: {{ j.last_error }}</p>
          </div>
          <div v-if="!mode.endsWith(`:${j.id}`)" class="flex flex-wrap gap-1">
            <Button size="sm" variant="outline" :disabled="j.running || !!busy" @click="runNow(j)"><LoaderCircle v-if="busy === j.id" class="animate-spin" /><Play v-else />{{ t("backup.runNow") }}</Button>
            <Button size="sm" variant="outline" :disabled="!j.copies.length" @click="start('restore', j)"><ArchiveRestore />{{ t("backup.restore") }}</Button>
            <Button size="sm" variant="ghost" :aria-label="t('backup.edit')" @click="start('edit', j)"><Settings2 /></Button>
            <Button size="sm" variant="ghost" :aria-label="t('backup.delete')" @click="start('delete', j)"><Trash2 /></Button>
          </div>
        </div>

        <!-- Ubah jadwal -->
        <form v-if="mode === `edit:${j.id}`" class="space-y-3 border-t pt-3" @submit.prevent="save(j)">
          <div class="grid gap-3 sm:grid-cols-2">
            <div class="space-y-1.5">
              <Label :for="`bk-h-${j.id}`">{{ t("backup.hour") }}</Label>
              <Input :id="`bk-h-${j.id}`" v-model="form.hour" inputmode="numeric" autocomplete="off" />
            </div>
            <div class="space-y-1.5">
              <Label :for="`bk-k-${j.id}`">{{ t("backup.keep") }}</Label>
              <Input :id="`bk-k-${j.id}`" v-model="form.keep" inputmode="numeric" autocomplete="off" />
            </div>
          </div>
          <p v-if="keepOK && Number(form.keep) < j.copies.length" class="flex items-center gap-1 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlert class="size-3.5" />{{ t("backup.keepWarning", { n: j.copies.length - Number(form.keep) }) }}
          </p>
          <label class="flex items-center gap-2 text-sm"><input v-model="form.enabled" type="checkbox" class="size-4 accent-primary" />{{ t("backup.enabled") }}</label>
          <div class="space-y-1.5">
            <Label :for="`bk-p-${j.id}`">{{ t("account.password") }}</Label>
            <Input :id="`bk-p-${j.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="!!busy || !form.password || !hourOK || !keepOK"><LoaderCircle v-if="busy === j.id" class="animate-spin" />{{ t("backup.save") }}</Button>
            <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
          </div>
        </form>

        <!-- Pulihkan sebagai folder baru -->
        <form v-if="mode === `restore:${j.id}`" class="space-y-3 border-t pt-3" @submit.prevent="restore(j)">
          <p class="text-xs text-muted-foreground">{{ t("backup.restoreHelp") }}</p>
          <div class="grid gap-3 sm:grid-cols-2">
            <div class="space-y-1.5">
              <Label :for="`bk-c-${j.id}`">{{ t("backup.copy") }}</Label>
              <select
                :id="`bk-c-${j.id}`"
                v-model="form.copy"
                class="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm shadow-xs focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
              >
                <option v-for="c in [...j.copies].reverse()" :key="c.name" :value="c.name">{{ when(c.created) }}</option>
              </select>
            </div>
            <div class="space-y-1.5">
              <Label :for="`bk-n-${j.id}`">{{ t("backup.newName") }}</Label>
              <Input :id="`bk-n-${j.id}`" v-model="form.name" pattern="^[a-z][a-z0-9_-]{0,31}$" autocomplete="off" />
            </div>
          </div>
          <div class="space-y-1.5">
            <Label :for="`bk-rp-${j.id}`">{{ t("account.password") }}</Label>
            <Input :id="`bk-rp-${j.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="!!busy || !form.password || !form.copy || !form.name.trim()"><LoaderCircle v-if="busy === j.id" class="animate-spin" />{{ t("backup.restore") }}</Button>
            <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
          </div>
        </form>

        <!-- Hapus tugas -->
        <form v-if="mode === `delete:${j.id}`" class="space-y-3 border-t pt-3" @submit.prevent="remove(j)">
          <p class="text-xs text-muted-foreground">{{ t("backup.deleteHelp") }}</p>
          <label class="flex items-center gap-2 text-sm"><input v-model="form.deleteData" type="checkbox" class="size-4 accent-primary" />{{ t("backup.deleteData", { pool: j.dest_pool }) }}</label>
          <div v-if="form.deleteData" class="space-y-1.5">
            <Label :for="`bk-dc-${j.id}`">{{ t("backup.confirmId", { id: j.id }) }}</Label>
            <Input :id="`bk-dc-${j.id}`" v-model="form.confirm" autocomplete="off" class="max-w-xs" />
          </div>
          <div class="space-y-1.5">
            <Label :for="`bk-dp-${j.id}`">{{ t("account.password") }}</Label>
            <Input :id="`bk-dp-${j.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" variant="destructive" :disabled="!!busy || !form.password || (form.deleteData && form.confirm !== j.id)">
              <LoaderCircle v-if="busy === j.id" class="animate-spin" />{{ t("backup.delete") }}
            </Button>
            <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
          </div>
        </form>
      </li>
    </ul>

    <section v-if="restores.length" class="space-y-2">
      <h3 class="text-sm font-semibold">{{ t("backup.restores") }}</h3>
      <ul class="space-y-1 text-sm">
        <li v-for="r in restores" :key="r.started + r.name" class="flex items-center gap-2">
          <LoaderCircle v-if="r.running" class="size-4 animate-spin" />
          <CircleCheck v-else-if="r.ok" class="size-4 text-emerald-600" />
          <TriangleAlert v-else class="size-4 text-destructive" />
          <span>{{ t("backup.restoreRow", { name: r.name, when: when(r.started) }) }}</span>
          <span v-if="r.error" class="text-xs text-destructive">{{ r.error }}</span>
        </li>
      </ul>
    </section>
  </div>
</template>
