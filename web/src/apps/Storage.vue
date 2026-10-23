<script setup lang="ts">
import { onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { HardDrive, RefreshCw, Thermometer, Timer, TriangleAlert } from "@lucide/vue"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import type { Disk, Smart, SmartStatus } from "@/lib/api"
import { api, ApiError } from "@/lib/api"
import { formatBytes } from "@/lib/format"

const { t, locale } = useI18n()
const disks = ref<Disk[] | null>(null)
const error = ref<string | null>(null)
const loading = ref(false)
// SMART dibaca per disk setelah daftar tampil, agar panel tidak menunggu
// disk yang lambat. Nilai null = sedang dibaca.
const smart = reactive<Record<string, Smart | null>>({})
const expanded = reactive<Record<string, boolean>>({})

async function load() {
  loading.value = true
  try {
    const list = await api.disks()
    disks.value = list
    error.value = null
    for (const d of list) loadSmart(d.name)
  } catch (e) {
    disks.value = null
    if (e instanceof ApiError && e.code === "agent_unavailable") error.value = t("shell.agentDown")
    else if (e instanceof ApiError && e.code === "unavailable") error.value = t("storage.unavailable")
    else error.value = t("common.error")
  } finally {
    loading.value = false
  }
}

async function loadSmart(name: string) {
  smart[name] = null
  try {
    smart[name] = await api.diskSmart(name)
  } catch (e) {
    smart[name] = { device: name, status: "unavailable", message: e instanceof Error ? e.message : "", attributes: [] }
  }
}

onMounted(load)

function kind(d: Disk) {
  if (d.transport === "nvme" || d.name.startsWith("nvme")) return t("storage.kind.nvme")
  return d.rotational ? t("storage.kind.hdd") : t("storage.kind.ssd")
}

const badgeVariant: Record<SmartStatus, "default" | "secondary" | "destructive" | "outline"> = {
  passed: "secondary",
  failed: "destructive",
  standby: "outline",
  unsupported: "outline",
  unavailable: "outline",
}

const statusHelp: Partial<Record<SmartStatus, string>> = {
  failed: "storage.smart.failedHelp",
  standby: "storage.smart.standbyHelp",
  unsupported: "storage.smart.unsupportedHelp",
}

const size = (bytes: number) => formatBytes(bytes, locale.value, 1000)
const num = (n: number) => new Intl.NumberFormat(locale.value).format(n)
</script>

<template>
  <div class="space-y-4 p-4">
    <div class="flex items-start justify-between gap-3">
      <p class="text-sm text-muted-foreground">{{ t("storage.intro") }}</p>
      <Button size="sm" variant="ghost" :disabled="loading" @click="load">
        <RefreshCw :class="loading && 'animate-spin'" />{{ t("storage.refresh") }}
      </Button>
    </div>

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
          <Skeleton v-if="smart[d.name] === null" class="h-5 w-20 rounded-full" :aria-label="t('storage.smart.loading')" />
          <Badge v-else-if="smart[d.name]" :variant="badgeVariant[smart[d.name]!.status]">
            <TriangleAlert v-if="smart[d.name]!.status === 'failed'" />
            {{ t(`storage.smart.${smart[d.name]!.status}`) }}
          </Badge>
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
            :class="smart[d.name]!.status === 'failed' ? 'border border-destructive/40 bg-destructive/5 font-medium' : 'bg-muted text-muted-foreground'"
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
                  <tr v-for="a in smart[d.name]!.attributes" :key="`${a.id ?? ''}-${a.name}`" :class="a.failing && 'bg-destructive/10 font-medium text-destructive'">
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
          </div>
        </div>
      </li>
    </ul>

    <div v-if="!disks && !error" class="space-y-3">
      <Skeleton v-for="i in 2" :key="i" class="h-36 rounded-xl" />
    </div>
  </div>
</template>
