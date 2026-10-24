<script setup lang="ts">
// Shared folder (hanya admin): daftar folder, cara membukanya dari komputer
// lain, izin per pengguna dan grup, dan hapus folder. Folder baru dibuat
// lewat wizard (wizards/share-create.yaml).
import { onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, Copy, FolderLock, LoaderCircle, Plus, RefreshCw, Trash2, TriangleAlert } from "@lucide/vue"
import PermissionEditor from "@/components/PermissionEditor.vue"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { useSession } from "@/composables/useSession"
import type { PoolsResult, ShareAccess, SharedFolder, UserAccount, UserGroup, Wizard } from "@/lib/api"
import { api, ApiError, wizardActions } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import WizardRunner from "@/wizard/WizardRunner.vue"

const { t, locale } = useI18n()
const { policy } = useSession()

const shares = ref<SharedFolder[] | null>(null)
const users = ref<UserAccount[]>([])
const groups = ref<UserGroup[]>([])
const pools = ref<PoolsResult | null>(null)
const loadError = ref("")
const notice = ref("")
const wizard = ref<Wizard | null>(null)

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`, { message: e instanceof Error ? e.message : "" }) : t("errors.generic")
}

async function load() {
  try {
    ;[shares.value, users.value, groups.value, pools.value] = await Promise.all([api.shares(), api.users(), api.groups(), api.pools()])
    loadError.value = ""
  } catch (e) {
    loadError.value = message(e)
  }
}
onMounted(load)

const size = (b: number) => formatBytes(b, locale.value, 1000)
const host = window.location.hostname
const paths = (s: SharedFolder) => [`\\\\${host}\\${s.name}`, `smb://${host}/${s.name}`]
async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
  } catch {
    // clipboard tidak tersedia
  }
}

function summary(p: Record<string, ShareAccess>) {
  const list = Object.entries(p).map(([who, l]) => `${who.split(":")[1]} (${t(`permissions.${l}`)})`)
  return list.length ? list.join(", ") : t("permissions.nobodyShort")
}

// --- Buat (wizard) ---
async function openWizard() {
  notice.value = ""
  await load()
  try {
    wizard.value = await api.wizard("share-create")
  } catch (e) {
    loadError.value = message(e)
  }
}
// Folder sudah tersimpan walau belum bisa disiapkan di sistem (misalnya
// sapod sibuk): wizard tetap selesai, pesannya tampil di daftar.
let lastError = ""
async function submitWizard(values: Record<string, string>) {
  const res = (await wizardActions[wizard.value!.action]!(values, {})) as SharedFolder
  lastError = res.error ?? ""
  return res
}
function wizardDone(values: Record<string, string>) {
  wizard.value = null
  if (lastError) loadError.value = t("errors.share_not_ready", { message: lastError === "system_sync_failed" ? t("errors.system_sync_failed") : lastError })
  else notice.value = t("shares.created", { name: values.name })
  load()
}

// --- Ubah izin dan hapus ---
const open = ref("")
const mode = ref<"" | "edit" | "delete">("")
const busy = ref(false)
const error = ref("")
const form = reactive({ permissions: {} as Record<string, ShareAccess>, adminPassword: "", confirmName: "" })

function start(s: SharedFolder, m: "edit" | "delete") {
  open.value = s.name
  mode.value = m
  error.value = ""
  notice.value = ""
  Object.assign(form, { permissions: { ...s.permissions }, adminPassword: "", confirmName: "" })
}
async function run(fn: () => Promise<unknown>, done: string) {
  busy.value = true
  error.value = ""
  try {
    await fn()
    notice.value = done
    open.value = ""
    await load()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}
const save = (s: SharedFolder) =>
  run(() => api.updateShare(s.name, { permissions: form.permissions, admin_password: form.adminPassword }), t("shares.saved", { name: s.name }))
const remove = (s: SharedFolder) =>
  run(() => api.deleteShare(s.name, { confirm_name: form.confirmName, admin_password: form.adminPassword }), t("shares.deleted", { name: s.name }))
</script>

<template>
  <div v-if="wizard" class="p-4">
    <h2 class="mb-4 text-sm font-medium text-muted-foreground">{{ wizard.title[locale] ?? wizard.title.en }}</h2>
    <WizardRunner
      :wizard="wizard"
      :policy="policy"
      :submit="submitWizard"
      :context="{ users, groups, pools: pools ?? undefined }"
      cancelable
      @cancel="wizard = null"
      @done="wizardDone"
    />
  </div>

  <div v-else class="space-y-4 p-4">
    <div class="flex items-start justify-between gap-3">
      <p class="text-sm text-muted-foreground">{{ t("shares.intro") }}</p>
      <div class="flex shrink-0 gap-2">
        <Button size="sm" variant="ghost" @click="load"><RefreshCw />{{ t("storage.refresh") }}</Button>
        <Button size="sm" :disabled="!pools?.pools.length" @click="openWizard"><Plus />{{ t("shares.add") }}</Button>
      </div>
    </div>
    <p v-if="pools && !pools.pools.length" class="rounded-lg bg-muted p-3 text-sm">{{ t("shares.noPool") }}</p>
    <p v-if="notice" class="flex items-center gap-2 rounded-lg border border-emerald-500/40 bg-emerald-500/5 p-3 text-sm">
      <CircleCheck class="size-4 shrink-0 text-emerald-600" />{{ notice }}
    </p>
    <p v-if="loadError" class="rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm">{{ loadError }}</p>
    <p v-if="shares && !shares.length" class="rounded-xl border p-4 text-sm text-muted-foreground">{{ t("shares.none") }}</p>

    <ul v-if="shares?.length" class="space-y-3">
      <li v-for="s in shares" :key="s.name" class="space-y-3 rounded-xl border p-4">
        <div class="flex flex-wrap items-start justify-between gap-2">
          <div class="flex min-w-0 items-start gap-3">
            <FolderLock class="mt-0.5 size-5 shrink-0 text-muted-foreground" />
            <div class="min-w-0 space-y-0.5">
              <div class="font-medium">{{ s.name }}</div>
              <p v-if="s.description" class="text-xs text-muted-foreground">{{ s.description }}</p>
              <p class="text-xs text-muted-foreground">
                {{ t("shares.onPool", { pool: s.pool }) }} · {{ t("shares.used", { used: size(s.used_bytes), free: size(s.available_bytes) }) }}
              </p>
              <p class="text-xs">{{ t("shares.access") }}: {{ summary(s.permissions) }}</p>
            </div>
          </div>
          <div v-if="open !== s.name" class="flex gap-1">
            <Button size="sm" variant="outline" @click="start(s, 'edit')">{{ t("shares.editAccess") }}</Button>
            <Button size="sm" variant="ghost" class="text-destructive hover:text-destructive" @click="start(s, 'delete')"><Trash2 /></Button>
          </div>
        </div>

        <p v-if="s.acl_pending" class="flex items-center gap-2 rounded-lg bg-muted p-2.5 text-xs">
          <LoaderCircle class="size-4 shrink-0 animate-spin" />{{ t("shares.aclPending") }}
        </p>
        <p v-else-if="s.acl_error" class="rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-xs">{{ t("shares.aclError", { message: s.acl_error }) }}</p>
        <p v-if="!Object.keys(s.permissions).length" class="flex items-center gap-2 rounded-lg border border-amber-500/40 bg-amber-500/5 p-2.5 text-xs">
          <TriangleAlert class="size-4 shrink-0 text-amber-600" />{{ t("shares.notShared") }}
        </p>
        <div v-else class="space-y-1 rounded-lg bg-muted p-2.5 text-xs">
          <p class="text-muted-foreground">{{ t("shares.openFrom") }}</p>
          <p v-for="p in paths(s)" :key="p" class="flex items-center justify-between gap-2">
            <code class="font-mono break-all select-all">{{ p }}</code>
            <button type="button" class="text-muted-foreground hover:text-foreground" :aria-label="t('recovery.copy')" @click="copy(p)"><Copy class="size-3.5" /></button>
          </p>
        </div>

        <form v-if="open === s.name && mode === 'edit'" class="space-y-3 border-t pt-3" @submit.prevent="save(s)">
          <PermissionEditor v-model="form.permissions" :users="users" :groups="groups" />
          <div class="space-y-1.5">
            <Label :for="`sp-${s.name}`">{{ t("account.password") }}</Label>
            <Input :id="`sp-${s.name}`" v-model="form.adminPassword" type="password" autocomplete="current-password" />
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="busy || !form.adminPassword"><LoaderCircle v-if="busy" class="animate-spin" />{{ t("users.save") }}</Button>
            <Button type="button" size="sm" variant="ghost" @click="open = ''">{{ t("common.cancel") }}</Button>
          </div>
        </form>

        <form v-else-if="open === s.name && mode === 'delete'" class="space-y-3 border-t pt-3" @submit.prevent="remove(s)">
          <p class="rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-sm">{{ t("shares.deleteHelp", { name: s.name, used: size(s.used_bytes) }) }}</p>
          <div class="space-y-1.5">
            <Label :for="`sc-${s.name}`">{{ t("shares.typeName") }}</Label>
            <Input :id="`sc-${s.name}`" v-model="form.confirmName" autocomplete="off" />
          </div>
          <div class="space-y-1.5">
            <Label :for="`sd-${s.name}`">{{ t("account.password") }}</Label>
            <Input :id="`sd-${s.name}`" v-model="form.adminPassword" type="password" autocomplete="current-password" />
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" variant="destructive" :disabled="busy || form.confirmName !== s.name || !form.adminPassword">
              <LoaderCircle v-if="busy" class="animate-spin" />{{ t("shares.delete") }}
            </Button>
            <Button type="button" size="sm" variant="ghost" @click="open = ''">{{ t("common.cancel") }}</Button>
          </div>
        </form>
        <p v-if="open === s.name && error" class="text-sm text-destructive" role="alert">{{ error }}</p>
      </li>
    </ul>
    <Skeleton v-if="!shares && !loadError" class="h-32 rounded-xl" />
  </div>
</template>
