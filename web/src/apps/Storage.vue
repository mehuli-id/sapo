<script setup lang="ts">
import { onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, HardDrive, Plus, RefreshCw, Thermometer, Timer, TriangleAlert } from "@lucide/vue"
import ExternalStorage from "@/components/ExternalStorage.vue"
import InstallPanel from "@/components/InstallPanel.vue"
import PoolCard from "@/components/PoolCard.vue"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { useSession } from "@/composables/useSession"
import type { Disk, Pool, PoolsResult, Smart, SmartStatus, Vdev, Wizard } from "@/lib/api"
import { api, ApiError, wizardActions } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { ineligibleReason } from "@/lib/pool"
import WizardRunner from "@/wizard/WizardRunner.vue"

const { t, locale } = useI18n()
const { policy } = useSession()
const disks = ref<Disk[] | null>(null)
const pools = ref<PoolsResult | null>(null)
const poolsError = ref<string | null>(null)
const scrubbing = reactive<Record<string, boolean>>({})
const actionError = ref("")
// Pesan berhasil terakhir (kunci i18n dan nama pool).
const done = ref<{ key: string; name: string } | null>(null)
// Wizard pool (buat, ganti disk, hapus) tampil menggantikan daftar selama
// dibuka, beserta pool dan disk yang sedang dikerjakan.
const wizard = ref<Wizard | null>(null)
const target = ref<{ pool?: Pool; vdev?: Vdev }>({})
const wizardFailed = ref(false)
const error = ref<string | null>(null)
const loading = ref(false)
// SMART dibaca per disk setelah daftar tampil, agar panel tidak menunggu
// disk yang lambat. Nilai null = sedang dibaca.
const smart = reactive<Record<string, Smart | null>>({})
const expanded = reactive<Record<string, boolean>>({})
// Nomor muat: jawaban dari muat sebelumnya yang datang terlambat diabaikan.
let generation = 0

async function load() {
  if (loading.value) return
  loading.value = true
  const gen = ++generation
  loadPools(gen)
  try {
    const list = await api.disks()
    if (gen !== generation) return
    disks.value = list
    error.value = null
    for (const d of list) loadSmart(d.name, gen)
  } catch (e) {
    disks.value = null
    error.value = errorText(e, "storage.unavailable")
  } finally {
    loading.value = false
  }
}

function errorText(e: unknown, unavailableKey: string) {
  if (e instanceof ApiError && e.code === "agent_unavailable") return t("shell.agentDown")
  if (e instanceof ApiError && e.code === "unavailable") return t(unavailableKey)
  if (e instanceof ApiError && e.code === "busy") return t("errors.busy")
  return t("common.error")
}

async function loadPools(gen: number) {
  try {
    const res = await api.pools()
    if (gen !== generation) return
    pools.value = res
    poolsError.value = null
  } catch (e) {
    if (gen !== generation) return
    pools.value = null
    poolsError.value = errorText(e, "pool.unavailable")
  }
}

async function scrub(name: string, action: "start" | "stop") {
  scrubbing[name] = true
  actionError.value = ""
  try {
    await api.scrubPool(name, action)
    await loadPools(generation)
  } catch (e) {
    const code = e instanceof ApiError ? e.code : "generic"
    actionError.value = t(`errors.${code}`) !== `errors.${code}` ? t(`errors.${code}`, { message: e instanceof Error ? e.message : "" }) : t("errors.generic")
  } finally {
    scrubbing[name] = false
  }
}

async function openWizard(id: "pool-create" | "pool-replace" | "pool-destroy" | "install", on: { pool?: Pool; vdev?: Vdev } = {}) {
  wizardFailed.value = false
  done.value = null
  try {
    // Daftar disk dibaca ulang: wizard harus memakai keadaan terbaru.
    await load()
    target.value = on
    wizard.value = await api.wizard(id)
  } catch {
    wizardFailed.value = true
  }
}

const wizardContext = () => ({ disks: disks.value ?? [], pools: pools.value ?? undefined, ...target.value })

async function submitWizard(values: Record<string, string>) {
  const action = wizardActions[wizard.value!.action]
  if (!action) throw new Error(`tindakan wizard tidak dikenal: ${wizard.value!.action}`)
  return action(values, wizardContext())
}

const doneKey: Record<string, string> = {
  "storage.pool_create": "pool.created",
  "storage.pool_replace": "pool.replaced",
  "storage.pool_destroy": "pool.destroyed",
  "system.install": "install.started",
}

function wizardDone(values: Record<string, string>) {
  done.value = { key: doneKey[wizard.value!.action] ?? "pool.created", name: values.name || target.value.pool?.name || values.disk || "" }
  wizard.value = null
  target.value = {}
  load()
  setTimeout(() => installPanel.value?.load(), 500)
}
const installPanel = ref<InstanceType<typeof InstallPanel> | null>(null)

const usableDisks = () => (disks.value ?? []).filter((d) => !ineligibleReason(d, !!pools.value?.allow_removable)).length

async function loadSmart(name: string, gen: number) {
  smart[name] = null
  let result: Smart
  try {
    result = await api.diskSmart(name)
  } catch (e) {
    result = { device: name, status: "unavailable", message: e instanceof Error ? e.message : "", attributes: [] }
  }
  if (gen !== generation) return
  smart[name] = result
  // Disk bermasalah: tabel atribut langsung terbuka.
  if (result.status === "failed" || result.status === "warning") expanded[name] = true
}

onMounted(load)

function kind(d: Disk) {
  if (d.transport === "nvme" || d.name.startsWith("nvme")) return t("storage.kind.nvme")
  return d.rotational ? t("storage.kind.hdd") : t("storage.kind.ssd")
}

const badgeVariant: Record<SmartStatus, "default" | "secondary" | "destructive" | "outline"> = {
  passed: "secondary",
  warning: "outline",
  failed: "destructive",
  standby: "outline",
  unsupported: "outline",
  unavailable: "outline",
}

const statusHelp: Partial<Record<SmartStatus, string>> = {
  failed: "storage.smart.failedHelp",
  warning: "storage.smart.warningHelp",
  standby: "storage.smart.standbyHelp",
  unsupported: "storage.smart.unsupportedHelp",
}

const size = (bytes: number) => formatBytes(bytes, locale.value, 1000)
const num = (n: number) => new Intl.NumberFormat(locale.value).format(n)
</script>

<template>
  <div v-if="wizard" class="p-4">
    <h2 class="mb-4 text-sm font-medium text-muted-foreground">
      <template v-if="target.vdev && target.pool">{{ t("pool.replaceTitle", { disk: target.vdev.disk || target.vdev.name, pool: target.pool.name }) }}</template>
      <template v-else-if="target.pool">{{ t("pool.destroyTitle", { pool: target.pool.name }) }}</template>
      <template v-else>{{ wizard.title[locale] ?? wizard.title.en }}</template>
    </h2>
    <WizardRunner
      :wizard="wizard"
      :policy="policy"
      :submit="submitWizard"
      :context="wizardContext()"
      cancelable
      @cancel="wizard = null"
      @done="wizardDone"
    />
  </div>

  <div v-else class="space-y-4 p-4">
    <div class="flex items-start justify-between gap-3">
      <p class="text-sm text-muted-foreground">{{ t("storage.intro") }}</p>
      <Button size="sm" variant="ghost" :disabled="loading" @click="load">
        <RefreshCw :class="loading && 'animate-spin'" />{{ t("storage.refresh") }}
      </Button>
    </div>

    <InstallPanel ref="installPanel" @install="openWizard('install')" />

    <!-- Pool -->
    <section class="space-y-3">
      <div class="flex items-center justify-between gap-3">
        <h3 class="text-sm font-semibold">{{ t("pool.title") }}</h3>
        <Button v-if="pools?.available" size="sm" :disabled="!usableDisks()" @click="openWizard('pool-create')"><Plus />{{ t("pool.create") }}</Button>
      </div>
      <p v-if="done" class="flex items-center gap-2 rounded-lg border border-emerald-500/40 bg-emerald-500/5 p-3 text-sm">
        <CircleCheck class="size-4 shrink-0 text-emerald-600" />{{ t(done.key, { name: done.name }) }}
      </p>
      <p v-if="wizardFailed || actionError" class="rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm">{{ actionError || t("errors.generic") }}</p>
      <p v-if="poolsError" class="rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm">{{ poolsError }}</p>
      <div v-else-if="pools && !pools.available" class="space-y-1 rounded-lg bg-muted p-3 text-sm">
        <p>{{ t("pool.zfsMissing") }}</p>
        <p v-if="pools.message" class="font-mono text-xs break-all text-muted-foreground">{{ pools.message }}</p>
      </div>
      <p v-else-if="pools && !pools.pools.length" class="rounded-xl border p-4 text-sm text-muted-foreground">
        {{ usableDisks() ? t("pool.none") : t("pool.noneNoDisks") }}
      </p>
      <ul v-if="pools?.pools.length" class="space-y-3">
        <PoolCard
          v-for="p in pools.pools"
          :key="p.name"
          :pool="p"
          :busy="!!scrubbing[p.name]"
          @scrub="(a) => scrub(p.name, a)"
          @replace="(v) => openWizard('pool-replace', { pool: p, vdev: v })"
          @destroy="openWizard('pool-destroy', { pool: p })"
        />
      </ul>
      <Skeleton v-if="!pools && !poolsError" class="h-24 rounded-xl" />
    </section>

    <ExternalStorage />

    <h3 class="pt-2 text-sm font-semibold">{{ t("storage.disksTitle") }}</h3>

    <div v-if="error" class="flex items-center justify-between gap-3 rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm">
      <span>{{ error }}</span>
      <Button size="sm" variant="outline" @click="load">{{ t("common.retry") }}</Button>
    </div>

    <p v-if="disks && disks.length === 0" class="rounded-xl border p-6 text-center text-sm text-muted-foreground">
      {{ t("storage.empty") }}
    </p>

    <ul v-if="disks?.length" class="space-y-3">
      <li v-for="d in disks" :key="d.name" class="space-y-3 rounded-xl border p-4">
        <div class="flex items-start justify-between gap-3">
          <div class="flex min-w-0 items-start gap-3">
            <HardDrive class="mt-0.5 size-5 shrink-0 text-muted-foreground" />
            <div class="min-w-0">
              <div class="truncate font-medium">{{ d.model || d.name }}</div>
              <div class="text-xs text-muted-foreground">
                {{ size(d.size_bytes) }} · {{ kind(d) }}<template v-if="d.transport"> · {{ d.transport.toUpperCase() }}</template> ·
                <code class="font-mono">{{ d.path }}</code>
              </div>
              <div v-if="d.serial" class="text-xs text-muted-foreground">
                {{ t("storage.serial") }}: <span class="font-mono">{{ d.serial }}</span>
              </div>
            </div>
          </div>
          <div class="flex shrink-0 flex-col items-end gap-1">
          <Skeleton v-if="smart[d.name] === null" class="h-5 w-20 rounded-full" :aria-label="t('storage.smart.loading')" />
          <Badge
            v-else-if="smart[d.name]"
            :variant="badgeVariant[smart[d.name]!.status]"
            :class="smart[d.name]!.status === 'warning' && 'border-amber-500/60 text-amber-700 dark:text-amber-400'"
          >
            <TriangleAlert v-if="smart[d.name]!.status === 'failed' || smart[d.name]!.status === 'warning'" />
            {{ t(`storage.smart.${smart[d.name]!.status}`) }}
          </Badge>
          <span class="text-xs text-muted-foreground">{{ t(d.usage === "in_use" ? `storage.usage.${d.usage_reason ?? "mounted"}` : `storage.usage.${d.usage}`) }}</span>
          </div>
        </div>

        <template v-if="smart[d.name]">
          <div
            v-if="smart[d.name]!.temperature_c !== undefined || smart[d.name]!.power_on_hours !== undefined"
            class="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground"
          >
            <span v-if="smart[d.name]!.temperature_c !== undefined" class="flex items-center gap-1">
              <Thermometer class="size-3.5" />{{ t("storage.temperature", { n: smart[d.name]!.temperature_c }) }}
            </span>
            <span v-if="smart[d.name]!.power_on_hours !== undefined" class="flex items-center gap-1">
              <Timer class="size-3.5" />{{ t("storage.powerOnHours", { n: num(smart[d.name]!.power_on_hours!) }) }}
            </span>
          </div>
          <p
            v-if="statusHelp[smart[d.name]!.status]"
            class="rounded-lg p-2.5 text-xs"
            :class="{
              'border border-destructive/40 bg-destructive/5 font-medium': smart[d.name]!.status === 'failed',
              'border border-amber-500/40 bg-amber-500/5 font-medium': smart[d.name]!.status === 'warning',
              'bg-muted text-muted-foreground': smart[d.name]!.status !== 'failed' && smart[d.name]!.status !== 'warning',
            }"
          >
            {{ t(statusHelp[smart[d.name]!.status]!) }}
          </p>
          <p v-else-if="smart[d.name]!.status === 'unavailable' && smart[d.name]!.message" class="rounded-lg bg-muted p-2.5 font-mono text-xs break-all text-muted-foreground">
            {{ smart[d.name]!.message }}
          </p>
        </template>

        <div class="space-y-1.5">
          <h4 class="text-xs font-semibold tracking-wide text-muted-foreground uppercase">{{ t("storage.partitions") }}</h4>
          <p v-if="!d.partitions.length" class="text-xs text-muted-foreground">{{ t("storage.noPartitions") }}</p>
          <ul v-else class="divide-y rounded-lg border text-xs">
            <li v-for="p in d.partitions" :key="p.name" class="flex flex-wrap items-center justify-between gap-x-3 gap-y-0.5 px-2.5 py-1.5">
              <span class="font-mono">{{ p.name }}</span>
              <span class="text-muted-foreground">
                {{ size(p.size_bytes) }}<template v-if="p.fstype"> · {{ p.fstype }}</template><template v-if="p.label"> · {{ p.label }}</template>
                <template v-for="m in p.mountpoints" :key="m"> · {{ t("storage.mountedAt", { path: m }) }}</template>
              </span>
            </li>
          </ul>
        </div>

        <div v-if="smart[d.name]?.attributes.length">
          <Button size="sm" variant="outline" @click="expanded[d.name] = !expanded[d.name]">
            {{ expanded[d.name] ? t("storage.smart.hideDetails") : t("storage.smart.showDetails") }}
          </Button>
          <div v-if="expanded[d.name]" class="mt-2 space-y-1.5">
            <div class="overflow-x-auto rounded-lg border">
              <table class="w-full text-xs">
                <thead class="bg-muted/50 text-left text-muted-foreground">
                  <tr>
                    <th class="px-2.5 py-1.5 font-medium">{{ t("storage.smart.attribute") }}</th>
                    <th class="px-2.5 py-1.5 text-right font-medium">{{ t("storage.smart.value") }}</th>
                    <th class="px-2.5 py-1.5 text-right font-medium">{{ t("storage.smart.worst") }}</th>
                    <th class="px-2.5 py-1.5 text-right font-medium">{{ t("storage.smart.threshold") }}</th>
                    <th class="px-2.5 py-1.5 text-right font-medium">{{ t("storage.smart.raw") }}</th>
                  </tr>
                </thead>
                <tbody class="divide-y">
                  <tr
                    v-for="a in smart[d.name]!.attributes"
                    :key="`${a.id ?? ''}-${a.name}`"
                    :class="a.failing ? 'bg-destructive/10 font-medium text-destructive' : a.warning && 'bg-amber-500/10 font-medium text-amber-700 dark:text-amber-400'"
                  >
                    <td class="px-2.5 py-1.5"><span v-if="a.id" class="text-muted-foreground tabular-nums">{{ a.id }} </span>{{ a.name }}</td>
                    <td class="px-2.5 py-1.5 text-right tabular-nums">{{ a.value ?? "—" }}</td>
                    <td class="px-2.5 py-1.5 text-right tabular-nums">{{ a.worst ?? "—" }}</td>
                    <td class="px-2.5 py-1.5 text-right tabular-nums">{{ a.threshold ?? "—" }}</td>
                    <td class="px-2.5 py-1.5 text-right font-mono break-all">{{ a.raw }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
            <p v-if="smart[d.name]!.attributes.some((a) => a.failing)" class="text-xs text-destructive">{{ t("storage.smart.failingNote") }}</p>
            <p v-if="smart[d.name]!.attributes.some((a) => a.warning)" class="text-xs text-amber-700 dark:text-amber-400">{{ t("storage.smart.warningNote") }}</p>
          </div>
        </div>
      </li>
    </ul>

    <div v-if="!disks && !error" class="space-y-3">
      <Skeleton v-for="i in 2" :key="i" class="h-36 rounded-xl" />
    </div>
  </div>
</template>
