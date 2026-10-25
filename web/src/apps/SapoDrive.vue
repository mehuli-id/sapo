<script setup lang="ts">
// Sapo Drive (ADR 0013): folder pribadi setiap pengguna. Pengguna melihat
// pemakaian dan cara membukanya; admin menyalakan (wizard
// wizards/drive-enable.yaml), mengatur kuota, dan menghapus folder sisa
// pengguna yang sudah dihapus.
import { computed, onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, Copy, FolderOpen, LoaderCircle, Trash2, TriangleAlert } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { useLauncher } from "@/composables/useLauncher"
import { useSession } from "@/composables/useSession"
import type { DriveAdmin, DriveInfo, HomeStatus, PoolsResult, SnapshotPolicy, Wizard } from "@/lib/api"
import { api, ApiError, wizardActions } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import SnapshotPanel from "@/components/SnapshotPanel.vue"
import WizardRunner from "@/wizard/WizardRunner.vue"

const { t, locale } = useI18n()
const { status, policy } = useSession()
const { launch } = useLauncher()
const isAdmin = computed(() => status.value?.user?.role === "admin")
const info = ref<DriveInfo | null>(null)
const admin = ref<DriveAdmin | null>(null)
const pools = ref<PoolsResult | null>(null)
const error = ref("")
const notice = ref("")
const busy = ref(false)

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`, { message: e instanceof Error ? e.message : "" }) : t("errors.generic")
}

async function load() {
  try {
    info.value = await api.drive()
    if (isAdmin.value) [admin.value, pools.value] = await Promise.all([api.driveAdmin(), api.pools()])
  } catch (e) {
    error.value = message(e)
  }
}
onMounted(load)

const size = (b: number) => formatBytes(b, locale.value, 1000)
const host = window.location.hostname
const davURL = `${window.location.origin}/dav/`
const davGuides = ["windows", "macos", "ios", "android", "rclone"] as const
const smbPaths = computed(() => (info.value ? [`\\\\${host}\\${info.value.user}`, `smb://${host}/${info.value.user}`] : []))
async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
  } catch {
    // clipboard tidak tersedia
  }
}
function usage(h: HomeStatus) {
  return h.quota_bytes
    ? t("drive.usedOf", { used: size(h.used_bytes), quota: size(h.quota_bytes) })
    : t("drive.usedFree", { used: size(h.used_bytes), free: size(h.available_bytes) })
}
const percent = (h: HomeStatus) => (h.quota_bytes ? Math.min(100, Math.round((h.used_bytes / h.quota_bytes) * 100)) : 0)

// --- Nyalakan (wizard) ---
const wizard = ref<Wizard | null>(null)
async function openWizard() {
  error.value = notice.value = ""
  try {
    wizard.value = await api.wizard("drive-enable")
  } catch (e) {
    error.value = message(e)
  }
}
const submitWizard = (values: Record<string, string>) => wizardActions[wizard.value!.action]!(values, {})
function wizardDone() {
  wizard.value = null
  notice.value = t("drive.enabled")
  load()
}

// Jadwal snapshot semua folder pribadi.
const drivePolicy = (p: SnapshotPolicy | null) => api.setDrivePolicy(p)

// --- Pengaturan admin ---
const mode = ref("") // "default" | "quota:<user>" | "delete:<pool>/<user>" | "off"
const form = reactive({ quota: "", password: "", confirm: "" })
function start(m: string, quota = "") {
  mode.value = m
  error.value = notice.value = ""
  Object.assign(form, { quota, password: "", confirm: "" })
}
async function run(fn: () => Promise<{ warning?: string } | unknown>, done: string) {
  busy.value = true
  error.value = notice.value = ""
  try {
    const res = (await fn()) as { warning?: string }
    if (res?.warning) throw new ApiError(200, res.warning, "")
    notice.value = done
    mode.value = ""
    await load()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}
const quotaText = (q: number | undefined) =>
  q === undefined ? t("drive.quotaDefault") : q < 0 ? t("drive.noLimit") : t("drive.gb", { n: q })
const defaultQuota = computed(() => (admin.value?.quota_gb ? t("drive.gb", { n: admin.value.quota_gb }) : t("drive.noLimit")))
const saveDefault = () =>
  run(() => api.setDrive({ pool: admin.value!.pool, quota_gb: Number(form.quota || 0), admin_password: form.password }), t("drive.saved"))
const turnOff = () => run(() => api.setDrive({ pool: "", quota_gb: admin.value!.quota_gb, admin_password: form.password }), t("drive.disabled"))
// Kuota pengguna: kosong = bawaan, 0 = tanpa batas.
function saveQuota(user: string) {
  const q = form.quota.trim() === "" ? null : Number(form.quota) === 0 ? -1 : Number(form.quota)
  return run(() => api.setDriveQuota(user, { quota_gb: q, admin_password: form.password }), t("drive.saved"))
}
const removeHome = (h: HomeStatus) =>
  run(() => api.deleteHome(h.pool, h.user, { confirm_name: form.confirm, admin_password: form.password }), t("drive.homeDeleted", { user: h.user }))
</script>

<template>
  <div v-if="wizard" class="p-4">
    <h2 class="mb-4 text-sm font-medium text-muted-foreground">{{ wizard.title[locale] ?? wizard.title.en }}</h2>
    <WizardRunner :wizard="wizard" :policy="policy" :submit="submitWizard" :context="{ pools: pools ?? undefined }" cancelable @cancel="wizard = null" @done="wizardDone" />
  </div>

  <div v-else class="space-y-4 p-4">
    <p class="text-sm text-muted-foreground">{{ t("drive.intro") }}</p>
    <p v-if="notice" class="flex items-center gap-2 rounded-lg border border-emerald-500/40 bg-emerald-500/5 p-3 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ notice }}</p>
    <p v-if="error" class="flex items-center gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm" role="alert"><TriangleAlert class="size-4" />{{ error }}</p>
    <Skeleton v-if="!info && !error" class="h-32 rounded-xl" />

    <template v-if="info">
      <!-- Belum dinyalakan -->
      <div v-if="!info.enabled" class="space-y-3 rounded-xl border p-4">
        <p class="text-sm">{{ isAdmin ? t("drive.offAdmin") : t("drive.off") }}</p>
        <Button v-if="isAdmin" size="sm" :disabled="!pools?.pools.length" @click="openWizard">{{ t("drive.enable") }}</Button>
        <p v-if="isAdmin && pools && !pools.pools.length" class="text-xs text-muted-foreground">{{ t("shares.noPool") }}</p>
      </div>

      <!-- Drive saya -->
      <section v-else class="space-y-3 rounded-xl border p-4">
        <div class="flex flex-wrap items-start justify-between gap-2">
          <div class="space-y-0.5">
            <h3 class="font-medium">{{ t("files.home") }}</h3>
            <p v-if="info.home" class="text-xs text-muted-foreground">{{ usage(info.home) }}</p>
            <p v-else class="text-xs text-muted-foreground">{{ t("drive.preparing") }}</p>
          </div>
          <Button size="sm" variant="outline" @click="launch('file-station')"><FolderOpen />{{ t("drive.openFiles") }}</Button>
        </div>
        <div v-if="info.home?.quota_bytes" class="h-2 overflow-hidden rounded-full bg-muted" role="meter" :aria-valuenow="percent(info.home)" aria-valuemin="0" aria-valuemax="100">
          <div class="h-full rounded-full bg-primary" :style="{ width: `${percent(info.home)}%` }" />
        </div>
        <div v-if="info.smb" class="space-y-1.5">
          <p class="text-xs text-muted-foreground">{{ t("drive.smbHelp") }}</p>
          <div v-for="p in smbPaths" :key="p" class="flex items-center gap-2">
            <code class="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1 text-xs">{{ p }}</code>
            <button type="button" class="rounded p-1 hover:bg-accent" :aria-label="t('recovery.copy')" @click="copy(p)"><Copy class="size-3.5" /></button>
          </div>
        </div>
        <p v-else class="text-xs text-muted-foreground">{{ t("drive.smbOff") }}</p>
      </section>

      <!-- WebDAV (tahap 2): semua folder yang boleh dibuka, dari perangkat lain -->
      <section class="space-y-3 rounded-xl border p-4">
        <h3 class="font-medium">{{ t("drive.webdavTitle") }}</h3>
        <template v-if="info.webdav">
          <p class="text-xs text-muted-foreground">{{ t("drive.webdavHelp") }}</p>
          <div class="flex items-center gap-2">
            <code class="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1 text-xs">{{ davURL }}</code>
            <button type="button" class="rounded p-1 hover:bg-accent" :aria-label="t('recovery.copy')" @click="copy(davURL)"><Copy class="size-3.5" /></button>
          </div>
          <p class="text-xs">{{ t("drive.webdavLogin", { user: info.user }) }}</p>
          <Button size="sm" variant="outline" @click="launch('account-security')">{{ t("drive.webdavCreatePassword") }}</Button>
          <details v-for="os in davGuides" :key="os" class="rounded-lg border px-3 py-2 text-sm">
            <summary class="cursor-pointer font-medium">{{ t(`drive.guide.${os}.title`) }}</summary>
            <p class="mt-2 whitespace-pre-line text-xs text-muted-foreground">{{ t(`drive.guide.${os}.steps`, { url: davURL, user: info.user }) }}</p>
          </details>
          <p class="text-xs text-muted-foreground">{{ t("drive.webdavCert") }}</p>
          <a href="/api/v1/tls/certificate" download class="inline-block"><Button size="sm" variant="outline" type="button">{{ t("drive.downloadCert") }}</Button></a>
        </template>
        <p v-else class="text-xs text-muted-foreground">{{ t("drive.webdavOff") }}</p>
      </section>

      <!-- Admin -->
      <template v-if="isAdmin && admin && info.enabled">
        <section class="space-y-3 rounded-xl border p-4">
          <div class="flex flex-wrap items-start justify-between gap-2">
            <div class="space-y-0.5">
              <h3 class="font-medium">{{ t("drive.settings") }}</h3>
              <p class="text-xs text-muted-foreground">{{ t("drive.settingsSummary", { pool: admin.pool, quota: defaultQuota }) }}</p>
            </div>
            <div v-if="!mode" class="flex gap-1">
              <Button size="sm" variant="outline" @click="start('default', admin.quota_gb ? String(admin.quota_gb) : '')">{{ t("drive.changeDefault") }}</Button>
              <Button size="sm" variant="ghost" @click="start('off')">{{ t("drive.turnOff") }}</Button>
            </div>
          </div>
          <form v-if="mode === 'default' || mode === 'off'" class="space-y-3 border-t pt-3" @submit.prevent="mode === 'off' ? turnOff() : saveDefault()">
            <p v-if="mode === 'off'" class="text-sm">{{ t("drive.turnOffHelp") }}</p>
            <div v-else class="space-y-1.5">
              <Label for="dr-q">{{ t("drive.defaultQuota") }}</Label>
              <Input id="dr-q" v-model="form.quota" inputmode="numeric" pattern="[0-9]*" class="max-w-40" />
            </div>
            <div class="space-y-1.5">
              <Label for="dr-pw">{{ t("account.password") }}</Label>
              <Input id="dr-pw" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
            </div>
            <div class="flex gap-2">
              <Button type="submit" size="sm" :disabled="busy || !form.password"><LoaderCircle v-if="busy" class="animate-spin" />{{ mode === "off" ? t("drive.turnOff") : t("appCenter.save") }}</Button>
              <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
            </div>
          </form>
        </section>

        <section class="space-y-2 rounded-xl border p-4">
          <h3 class="font-medium">{{ t("snapshots.title") }}</h3>
          <SnapshotPanel policy-only :policy="admin.snapshots" :save="drivePolicy" @saved="load" />
          <p class="text-xs text-muted-foreground">{{ t("drive.snapshotsHelp") }}</p>
        </section>

        <section class="space-y-2 rounded-xl border p-4">
          <h3 class="font-medium">{{ t("drive.users") }}</h3>
          <ul class="divide-y">
            <li v-for="u in admin.users" :key="u.user" class="space-y-2 py-2">
              <div class="flex flex-wrap items-center justify-between gap-2">
                <div class="min-w-0">
                  <p class="text-sm">{{ u.user }}</p>
                  <p class="text-xs text-muted-foreground">
                    {{ u.home ? usage(u.home) : t("drive.preparing") }} · {{ t("drive.quota") }}: {{ quotaText(u.quota_gb) }}
                  </p>
                </div>
                <div v-if="!mode.endsWith(`:${u.user}`)" class="flex gap-1">
                  <Button v-if="u.home" size="sm" variant="outline" @click="start(`snap:${u.user}`)">{{ t("snapshots.title") }}</Button>
                  <Button size="sm" variant="outline" @click="start(`quota:${u.user}`, u.quota_gb === undefined ? '' : String(Math.max(0, u.quota_gb)))">
                    {{ t("drive.changeQuota") }}
                  </Button>
                </div>
              </div>
              <div v-if="mode === `snap:${u.user}` && u.home" class="space-y-2">
                <SnapshotPanel list-only :target="{ pool: u.home.pool, user: u.user }" />
                <Button size="sm" variant="ghost" @click="mode = ''">{{ t("common.close") }}</Button>
              </div>
              <form v-if="mode === `quota:${u.user}`" class="space-y-3" @submit.prevent="saveQuota(u.user)">
                <div class="space-y-1.5">
                  <Label :for="`dr-uq-${u.user}`">{{ t("drive.userQuota") }}</Label>
                  <Input :id="`dr-uq-${u.user}`" v-model="form.quota" inputmode="numeric" pattern="[0-9]*" class="max-w-40" />
                  <p class="text-xs text-muted-foreground">{{ t("drive.userQuotaHelp") }}</p>
                </div>
                <div class="space-y-1.5">
                  <Label :for="`dr-upw-${u.user}`">{{ t("account.password") }}</Label>
                  <Input :id="`dr-upw-${u.user}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
                </div>
                <div class="flex gap-2">
                  <Button type="submit" size="sm" :disabled="busy || !form.password"><LoaderCircle v-if="busy" class="animate-spin" />{{ t("appCenter.save") }}</Button>
                  <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
                </div>
              </form>
            </li>
          </ul>
        </section>

        <section v-if="admin.orphans.length" class="space-y-2 rounded-xl border p-4">
          <h3 class="font-medium">{{ t("drive.orphans") }}</h3>
          <p class="text-xs text-muted-foreground">{{ t("drive.orphansHelp") }}</p>
          <ul class="divide-y">
            <li v-for="h in admin.orphans" :key="`${h.pool}/${h.user}`" class="space-y-2 py-2">
              <div class="flex flex-wrap items-center justify-between gap-2">
                <div class="min-w-0">
                  <p class="text-sm">{{ h.user }}</p>
                  <p class="text-xs text-muted-foreground">{{ t("drive.onPool", { pool: h.pool }) }} · {{ size(h.used_bytes) }}</p>
                </div>
                <Button
                  v-if="mode !== `delete:${h.pool}/${h.user}` && !admin.users.some((u) => u.user === h.user)"
                  size="sm"
                  variant="ghost"
                  class="text-destructive hover:text-destructive"
                  :aria-label="t('drive.deleteHome')"
                  @click="start(`delete:${h.pool}/${h.user}`)"
                  ><Trash2
                /></Button>
              </div>
              <form v-if="mode === `delete:${h.pool}/${h.user}`" class="space-y-3" @submit.prevent="removeHome(h)">
                <p class="rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-sm">{{ t("drive.deleteHomeHelp", { user: h.user }) }}</p>
                <div class="space-y-1.5">
                  <Label :for="`dr-c-${h.user}`">{{ t("appCenter.typeId", { id: h.user }) }}</Label>
                  <Input :id="`dr-c-${h.user}`" v-model="form.confirm" autocomplete="off" class="max-w-xs" />
                </div>
                <div class="space-y-1.5">
                  <Label :for="`dr-dpw-${h.user}`">{{ t("account.password") }}</Label>
                  <Input :id="`dr-dpw-${h.user}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
                </div>
                <div class="flex gap-2">
                  <Button type="submit" size="sm" variant="destructive" :disabled="busy || form.confirm !== h.user || !form.password">
                    <LoaderCircle v-if="busy" class="animate-spin" />{{ t("drive.deleteHome") }}
                  </Button>
                  <Button type="button" size="sm" variant="ghost" @click="mode = ''">{{ t("common.cancel") }}</Button>
                </div>
              </form>
            </li>
          </ul>
        </section>
      </template>
    </template>
  </div>
</template>
