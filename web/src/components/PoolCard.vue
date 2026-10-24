<script setup lang="ts">
// Satu pool ZFS: kesehatan, kapasitas, disk penyusunnya, dan scrub.
import { computed } from "vue"
import { useI18n } from "vue-i18n"
import { Database, LoaderCircle, Play, RefreshCcw, Square, Trash2, TriangleAlert } from "@lucide/vue"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Progress } from "@/components/ui/progress"
import type { Pool, Vdev } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { cn } from "@/lib/utils"

const props = defineProps<{ pool: Pool; busy: boolean }>()
const emit = defineEmits<{ scrub: [action: "start" | "stop"]; replace: [vdev: Vdev]; destroy: [] }>()
const { t, locale } = useI18n()

const size = (b: number) => formatBytes(b, locale.value, 1000)
const date = (s: number) => new Intl.DateTimeFormat(locale.value, { dateStyle: "medium", timeStyle: "short" }).format(new Date(s * 1000))

// Kesehatan pool → tampilan. Selain ONLINE, pool butuh perhatian.
const health = computed(() => {
  switch (props.pool.health) {
    case "ONLINE":
      return { key: "online", variant: "secondary" as const }
    case "DEGRADED":
      return { key: "degraded", variant: "outline" as const }
    case "OFFLINE":
      return { key: "offline", variant: "outline" as const }
    default:
      return { key: "faulted", variant: "destructive" as const }
  }
})

const scan = computed(() => props.pool.scan)
const running = computed(() => scan.value?.state === "scanning")
const progress = computed(() => {
  const s = scan.value
  if (!s || !s.to_examine_bytes) return 0
  return Math.min(100, Math.round((s.examined_bytes / s.to_examine_bytes) * 100))
})

// Disk di pohon vdev (daun), beserta grup induknya untuk label.
const leaves = computed(() => {
  const out: { vdev: Vdev; group: string }[] = []
  const walk = (vs: Vdev[], group: string) => {
    for (const v of vs) {
      if (v.children?.length) walk(v.children, v.name)
      else out.push({ vdev: v, group })
    }
  }
  walk(props.pool.vdevs, "")
  return out
})
// Selama resilver, disk lain tidak diganti dulu (data sedang dibangun ulang).
const resilvering = computed(() => scan.value?.function === "resilver" && running.value)
const errors = (v: Vdev) => v.read_errors + v.write_errors + v.checksum_errors
const vdevBad = (v: Vdev) => v.state !== "ONLINE" || errors(v) > 0
</script>

<template>
  <li class="space-y-3 rounded-xl border p-4">
    <div class="flex items-start justify-between gap-3">
      <div class="flex min-w-0 items-start gap-3">
        <Database class="mt-0.5 size-5 shrink-0 text-muted-foreground" />
        <div class="min-w-0">
          <div class="truncate font-medium">{{ pool.name }}</div>
          <div class="text-xs text-muted-foreground">
            <template v-if="pool.layout">{{ t(`pool.layout.${pool.layout}.name`) }} · </template>{{ t("pool.disks", { n: leaves.length }, leaves.length) }}
          </div>
        </div>
      </div>
      <Badge :variant="health.variant" :class="health.key === 'degraded' && 'border-amber-500/60 text-amber-700 dark:text-amber-400'">
        <TriangleAlert v-if="health.key !== 'online'" />{{ t(`pool.health.${health.key}`) }}
      </Badge>
    </div>

    <p
      v-if="health.key !== 'online'"
      :class="cn('rounded-lg p-2.5 text-xs font-medium', health.key === 'faulted' ? 'border border-destructive/40 bg-destructive/5' : 'border border-amber-500/40 bg-amber-500/5')"
    >
      {{ t(`pool.health.${health.key}Help`) }}
    </p>
    <div v-if="pool.status || pool.action" class="space-y-1 rounded-lg bg-muted p-2.5 text-xs text-muted-foreground">
      <p class="font-medium text-foreground">{{ t("pool.zfsSays") }}</p>
      <p v-if="pool.status">{{ pool.status }}</p>
      <p v-if="pool.action">{{ pool.action }}</p>
    </div>

    <div class="space-y-1.5">
      <Progress :model-value="pool.capacity_pct" :class="pool.capacity_pct >= 90 ? '[&>*]:bg-destructive' : pool.capacity_pct >= 80 && '[&>*]:bg-amber-500'" />
      <div class="flex flex-wrap justify-between gap-x-3 text-xs text-muted-foreground">
        <span>{{ t("pool.used", { used: size(pool.allocated_bytes), total: size(pool.size_bytes) }) }}</span>
        <span>{{ t("pool.free", { free: size(pool.free_bytes) }) }}</span>
      </div>
      <p v-if="pool.capacity_pct >= 80" class="text-xs text-amber-700 dark:text-amber-400">{{ t("pool.almostFull") }}</p>
    </div>

    <div class="space-y-1.5">
      <h4 class="text-xs font-semibold tracking-wide text-muted-foreground uppercase">{{ t("pool.members") }}</h4>
      <ul class="divide-y rounded-lg border text-xs">
        <li v-for="{ vdev, group } in leaves" :key="`${group}/${vdev.name}`" :class="cn('flex flex-wrap items-center justify-between gap-x-3 gap-y-0.5 px-2.5 py-1.5', vdevBad(vdev) && 'bg-destructive/5')">
          <span class="font-mono">{{ vdev.disk || vdev.name }}</span>
          <span class="flex items-center gap-2">
            <span :class="vdevBad(vdev) ? 'font-medium text-destructive' : 'text-muted-foreground'">
              {{ t(`pool.vdevState.${vdev.state}`) !== `pool.vdevState.${vdev.state}` ? t(`pool.vdevState.${vdev.state}`) : vdev.state }}
              <template v-if="errors(vdev)"> · {{ t("pool.errors", { r: vdev.read_errors, w: vdev.write_errors, c: vdev.checksum_errors }) }}</template>
            </span>
            <Button
              v-if="vdev.guid && !resilvering"
              size="sm"
              :variant="vdevBad(vdev) ? 'default' : 'ghost'"
              class="h-6 px-2 text-xs"
              @click="emit('replace', vdev)"
            >
              <RefreshCcw class="size-3" />{{ t("pool.replace") }}
            </Button>
          </span>
        </li>
      </ul>
    </div>

    <div class="flex flex-wrap items-center justify-between gap-2">
      <div class="min-w-0 flex-1 space-y-1 text-xs text-muted-foreground">
        <template v-if="scan && running">
          <p>{{ t(`pool.scan.${scan.function}Running`, { pct: progress }) }}<template v-if="scan.paused"> · {{ t("pool.scan.paused") }}</template></p>
          <Progress :model-value="progress" class="h-1.5" />
        </template>
        <p v-else-if="scan && scan.state === 'finished'">
          {{ t(`pool.scan.${scan.function}Done`, { date: date(scan.end_time || scan.start_time) }) }} ·
          <span :class="scan.errors > 0 && 'font-medium text-destructive'">{{ t("pool.scan.errors", { n: scan.errors }, scan.errors) }}</span>
        </p>
        <p v-else-if="scan && scan.state === 'canceled'">{{ t("pool.scan.canceled", { date: date(scan.end_time || scan.start_time) }) }}</p>
        <p v-else>{{ t("pool.scan.never") }}</p>
      </div>
      <Button v-if="scan?.function !== 'resilver' || !running" size="sm" variant="outline" :disabled="busy" @click="emit('scrub', running ? 'stop' : 'start')">
        <LoaderCircle v-if="busy" class="animate-spin" /><Square v-else-if="running" /><Play v-else />
        {{ running ? t("pool.scan.stop") : t("pool.scan.start") }}
      </Button>
    </div>
    <div class="flex justify-end border-t pt-3">
      <Button size="sm" variant="ghost" class="text-destructive hover:text-destructive" @click="emit('destroy')">
        <Trash2 />{{ t("pool.destroy") }}
      </Button>
    </div>
  </li>
</template>
