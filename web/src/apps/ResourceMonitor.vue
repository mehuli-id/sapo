<script setup lang="ts">
// Resource Monitor (hanya admin): pemakaian CPU, memori, jaringan, dan disk
// beberapa menit terakhir. sapod mengambil sampel hanya selama aplikasi ini
// terbuka.
import { computed, onMounted, onUnmounted, ref } from "vue"
import { useI18n } from "vue-i18n"
import LineChart from "@/components/LineChart.vue"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import type { StatsSample } from "@/lib/api"
import { api, ApiError } from "@/lib/api"
import { formatBytes } from "@/lib/format"

const { t, locale } = useI18n()
const samples = ref<StatsSample[]>([])
const interval = ref(2)
const error = ref("")
const MAX = 300

// Sampel diambil per halaman dengan nomor urut terakhir (tidak bergantung
// pada jam). Permintaan berikutnya baru dijadwalkan setelah yang ini selesai.
async function poll() {
  try {
    for (;;) {
      const last = samples.value[samples.value.length - 1]
      const res = await api.systemStats(last?.seq ?? 0)
      interval.value = res.interval_seconds
      samples.value = [...samples.value, ...res.samples].slice(-MAX)
      if (res.samples.length < 120) break
    }
    error.value = ""
  } catch (e) {
    error.value = e instanceof ApiError && e.code === "agent_unavailable" ? t("shell.agentDown") : t("common.error")
  }
  if (!stopped) timer = window.setTimeout(poll, interval.value * 1000)
}
let timer: number | undefined
let stopped = false
onMounted(poll)
onUnmounted(() => {
  stopped = true
  window.clearTimeout(timer)
})

const times = computed(() => samples.value.map((s) => Date.parse(s.time)))
const pct = (v: number) => `${Math.round(v)}%`
const rate = (v: number) => `${formatBytes(v, locale.value, 1000)}/s`

// Antarmuka dan disk yang dipilih; warna mengikuti arah (masuk/keluar),
// tidak berubah saat pilihan berganti.
const netNames = computed(() => [...new Set(samples.value.flatMap((s) => s.net.map((n) => n.name)))].sort())
const diskNames = computed(() => [...new Set(samples.value.flatMap((s) => s.disk.map((n) => n.name)))].sort())
const net = ref("")
const disk = ref("")
const pick = (list: string[], cur: string) => (list.includes(cur) ? cur : (list[0] ?? ""))
function rates(kind: "net" | "disk", name: string, dir: "in" | "out") {
  return samples.value.map((s) => s[kind].find((r) => r.name === name)?.[dir] ?? 0)
}
const netSel = computed(() => pick(netNames.value, net.value))
const diskSel = computed(() => pick(diskNames.value, disk.value))
const latest = computed(() => samples.value[samples.value.length - 1])
</script>

<template>
  <div class="space-y-5 p-4">
    <p class="text-sm text-muted-foreground">{{ t("monitor.intro", { seconds: interval }) }}</p>
    <p v-if="error" class="rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm" role="alert">{{ error }}</p>
    <template v-if="samples.length > 1">
      <LineChart
        :title="t('monitor.cpu')"
        :times="times"
        :max="100"
        :format="pct"
        :series="[
          { name: t('monitor.cpuBusy'), color: 'var(--viz-1)', values: samples.map((s) => s.cpu_percent) },
          { name: t('monitor.cpuIOWait'), color: 'var(--viz-2)', values: samples.map((s) => s.iowait_percent) },
        ]"
      />
      <LineChart
        :title="t('monitor.memory', { total: formatBytes(latest?.memory_total_bytes ?? 0, locale) })"
        :times="times"
        :max="100"
        :format="pct"
        :series="[{ name: t('monitor.memoryUsed'), color: 'var(--viz-1)', values: samples.map((s) => (s.memory_total_bytes ? (100 * s.memory_used_bytes) / s.memory_total_bytes : 0)) }]"
      />
      <div v-if="netNames.length" class="space-y-2">
        <div v-if="netNames.length > 1" class="flex flex-wrap gap-1">
          <Button v-for="n in netNames" :key="n" size="sm" :variant="n === netSel ? 'default' : 'outline'" @click="net = n">{{ n }}</Button>
        </div>
        <LineChart
          :title="t('monitor.network', { name: netSel })"
          :times="times"
          :format="rate"
          :series="[
            { name: t('monitor.received'), color: 'var(--viz-1)', values: rates('net', netSel, 'in') },
            { name: t('monitor.sent'), color: 'var(--viz-2)', values: rates('net', netSel, 'out') },
          ]"
        />
      </div>
      <div v-if="diskNames.length" class="space-y-2">
        <div v-if="diskNames.length > 1" class="flex flex-wrap gap-1">
          <Button v-for="d in diskNames" :key="d" size="sm" :variant="d === diskSel ? 'default' : 'outline'" @click="disk = d">{{ d }}</Button>
        </div>
        <LineChart
          :title="t('monitor.disk', { name: diskSel })"
          :times="times"
          :format="rate"
          :series="[
            { name: t('monitor.read'), color: 'var(--viz-1)', values: rates('disk', diskSel, 'in') },
            { name: t('monitor.written'), color: 'var(--viz-2)', values: rates('disk', diskSel, 'out') },
          ]"
        />
      </div>
    </template>
    <div v-else-if="!error" class="space-y-3">
      <p class="text-sm text-muted-foreground">{{ t("monitor.collecting") }}</p>
      <Skeleton class="h-36 rounded-xl" />
    </div>
  </div>
</template>
