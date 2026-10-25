<script setup lang="ts">
// Backup dari NAS lain, sisi penerima (ADR 0019): partner (NAS pengirim)
// dibuat lewat wizards/backup-partner-add.yaml dan mendapat kode pemasangan
// sekali tampil. Salinan yang diterima bisa dipulihkan sebagai shared folder
// baru di NAS ini; hanya admin NAS ini yang bisa menghapusnya.
import { onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { ArchiveRestore, CircleCheck, Copy, Inbox, LoaderCircle, Plus, Trash2, TriangleAlert } from "@lucide/vue"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import type { BackupPartner, PoolsResult, Wizard } from "@/lib/api"
import { api, ApiError, wizardActions } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { useSession } from "@/composables/useSession"
import WizardRunner from "@/wizard/WizardRunner.vue"

const props = defineProps<{ pools: PoolsResult | null }>()
const emit = defineEmits<{ restored: [] }>()
const { t, locale } = useI18n()
const { policy } = useSession()
const partners = ref<BackupPartner[] | null>(null)
const error = ref("")
const notice = ref("")
const busy = ref("")
const code = ref("")

function show(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  error.value = t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`) : t("errors.generic")
}
async function load() {
  try {
    partners.value = await api.partners()
  } catch (e) {
    show(e)
  }
}
onMounted(load)

const size = (b: number) => formatBytes(b, locale.value, 1000)
const when = (s?: string) => (s && !s.startsWith("0001") ? new Date(s).toLocaleString(locale.value, { dateStyle: "medium", timeStyle: "short" }) : "")

const wizard = ref<Wizard | null>(null)
async function openWizard() {
  error.value = notice.value = code.value = ""
  try {
    wizard.value = await api.wizard("backup-partner-add")
  } catch (e) {
    show(e)
  }
}
// Kode pemasangan dari hasil aksi wizard (tampil sekali).
async function submitWizard(values: Record<string, string>) {
  const res = (await wizardActions[wizard.value!.action]!(values, {})) as { code: string }
  code.value = res.code
  return res
}
function wizardDone() {
  wizard.value = null
  load()
}
const copied = ref(false)
async function copyCode() {
  try {
    await navigator.clipboard.writeText(code.value)
    copied.value = true
  } catch {
    // clipboard tidak tersedia (misalnya bukan HTTPS): admin menyalin manual
  }
}

const mode = ref("")
const form = reactive({ copy: "", name: "", password: "", deleteData: false, confirm: "" })
function start(m: string, p: BackupPartner, job = "") {
  mode.value = `${m}:${p.id}:${job}`
  error.value = notice.value = ""
  const j = p.jobs.find((x) => x.job === job)
  Object.assign(form, { copy: j?.copies.at(-1)?.name ?? "", name: `${job.split("-")[0] || "pulih"}-pulih`.slice(0, 32), password: "", deleteData: false, confirm: "" })
}
async function run(key: string, fn: () => Promise<unknown>, done: string) {
  busy.value = key
  error.value = notice.value = ""
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
const restore = (p: BackupPartner, job: string) =>
  run(
    p.id,
    async () => {
      await api.restorePartnerCopy(p.id, job, { copy: form.copy, name: form.name.trim(), admin_password: form.password })
      emit("restored")
    },
    t("rbackup.restoreStarted", { name: form.name.trim() }),
  )
const remove = (p: BackupPartner) =>
  run(p.id, () => api.deletePartner(p.id, { delete_data: form.deleteData, confirm_name: form.confirm, admin_password: form.password }), t("rbackup.partnerDeleted"))
</script>

<template>
  <section class="space-y-3 border-t pt-4">
    <h3 class="flex items-center gap-2 text-sm font-semibold"><Inbox class="size-4" />{{ t("rbackup.recvTitle") }}</h3>
    <WizardRunner v-if="wizard" :wizard="wizard" :policy="policy" :submit="submitWizard" :context="{ pools: props.pools ?? undefined }" cancelable @cancel="wizard = null" @done="wizardDone" />
    <template v-else>
      <p class="text-xs text-muted-foreground">{{ t("rbackup.recvHelp") }}</p>
      <div v-if="code" class="space-y-2 rounded-lg border border-amber-500/40 bg-amber-500/5 p-3 text-sm">
        <p class="font-medium">{{ t("rbackup.codeTitle") }}</p>
        <p class="font-mono text-xs break-all select-all">{{ code }}</p>
        <p class="text-xs text-muted-foreground">{{ t("rbackup.codeHelp") }}</p>
        <div class="flex gap-2">
          <Button size="sm" variant="outline" @click="copyCode"><Copy />{{ copied ? t("rbackup.copied") : t("rbackup.copy") }}</Button>
          <Button size="sm" variant="ghost" @click="code = ''">{{ t("common.close") }}</Button>
        </div>
      </div>
      <p v-if="notice" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ notice }}</p>
      <p v-if="error" class="flex items-center gap-2 text-sm text-destructive" role="alert"><TriangleAlert class="size-4" />{{ error }}</p>
      <Button size="sm" variant="outline" :disabled="!props.pools?.pools.length" @click="openWizard"><Plus />{{ t("rbackup.addPartner") }}</Button>

      <ul class="space-y-3">
        <li v-for="p in partners ?? []" :key="p.id" class="space-y-2 rounded-xl border p-3">
          <div class="flex flex-wrap items-start justify-between gap-2">
            <div class="min-w-0 space-y-0.5">
              <p class="flex items-center gap-1.5 text-sm font-medium">
                {{ p.name }}<Badge v-if="p.revoked" variant="outline">{{ t("rbackup.revoked") }}</Badge>
              </p>
              <p class="text-xs text-muted-foreground">
                {{ t("rbackup.partnerInfo", { pool: p.pool, keep: p.keep }) }}<template v-if="p.quota_gb"> · {{ t("rbackup.quota", { gb: p.quota_gb }) }}</template><template v-if="when(p.last_seen)"> · {{ t("rbackup.lastSeen", { when: when(p.last_seen) }) }}</template>
              </p>
            </div>
            <Button v-if="!mode.startsWith(`delete:${p.id}`)" size="sm" variant="ghost" :aria-label="t('rbackup.deletePartner')" @click="start('delete', p)"><Trash2 /></Button>
          </div>
          <p v-if="!p.jobs.length" class="text-xs text-muted-foreground">{{ t("rbackup.noCopiesYet") }}</p>
          <div v-for="j in p.jobs" :key="j.job" class="space-y-2 rounded-lg border p-2.5">
            <div class="flex flex-wrap items-center justify-between gap-2 text-sm">
              <span>
                <span class="font-mono text-xs">{{ j.job }}</span>
                <span class="ml-2 text-xs text-muted-foreground">{{ t("backup.copies", { n: j.copies.length }) }} · {{ size(j.used_bytes) }}<template v-if="j.copies.length"> · {{ when(j.copies.at(-1)!.created) }}</template></span>
              </span>
              <Button v-if="mode !== `restore:${p.id}:${j.job}`" size="sm" variant="outline" :disabled="!j.copies.length" @click="start('restore', p, j.job)"><ArchiveRestore />{{ t("backup.restore") }}</Button>
            </div>
            <form v-if="mode === `restore:${p.id}:${j.job}`" class="space-y-3 border-t pt-3" @submit.prevent="restore(p, j.job)">
              <p class="text-xs text-muted-foreground">{{ t("rbackup.restoreHelp", { pool: p.pool }) }}</p>
              <div class="grid gap-3 sm:grid-cols-2">
                <div class="space-y-1.5">
                  <Label :for="`rr-c-${p.id}-${j.job}`">{{ t("backup.copy") }}</Label>
                  <select
                    :id="`rr-c-${p.id}-${j.job}`"
                    v-model="form.copy"
                    class="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm shadow-xs focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
                  >
                    <option v-for="c in [...j.copies].reverse()" :key="c.name" :value="c.name">{{ when(c.created) }}</option>
                  </select>
                </div>
                <div class="space-y-1.5">
                  <Label :for="`rr-n-${p.id}-${j.job}`">{{ t("backup.newName") }}</Label>
                  <Input :id="`rr-n-${p.id}-${j.job}`" v-model="form.name" pattern="^[a-z][a-z0-9_-]{0,31}$" autocomplete="off" />
                </div>
              </div>
              <div class="space-y-1.5">
                <Label :for="`rr-p-${p.id}-${j.job}`">{{ t("account.password") }}</Label>
                <Input :id="`rr-p-${p.id}-${j.job}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
              </div>
              <div class="flex gap-2">
                <Button type="submit" size="sm" :disabled="!!busy || !form.password || !form.copy || !form.name.trim()"><LoaderCircle v-if="busy === p.id" class="animate-spin" />{{ t("backup.restore") }}</Button>
                <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
              </div>
            </form>
          </div>
          <form v-if="mode.startsWith(`delete:${p.id}`)" class="space-y-3 border-t pt-3" @submit.prevent="remove(p)">
            <p class="text-xs text-muted-foreground">{{ t("rbackup.deletePartnerHelp") }}</p>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.deleteData" type="checkbox" class="size-4 accent-primary" />{{ t("rbackup.deletePartnerData", { pool: p.pool }) }}</label>
            <div v-if="form.deleteData" class="space-y-1.5">
              <Label :for="`rd-c-${p.id}`">{{ t("backup.confirmId", { id: p.name }) }}</Label>
              <Input :id="`rd-c-${p.id}`" v-model="form.confirm" autocomplete="off" class="max-w-xs" />
            </div>
            <div class="space-y-1.5">
              <Label :for="`rd-p-${p.id}`">{{ t("account.password") }}</Label>
              <Input :id="`rd-p-${p.id}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
            </div>
            <div class="flex gap-2">
              <Button type="submit" size="sm" variant="destructive" :disabled="!!busy || !form.password || (form.deleteData && form.confirm !== p.name)">{{ t("rbackup.deletePartner") }}</Button>
              <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
            </div>
          </form>
        </li>
      </ul>
    </template>
  </section>
</template>
