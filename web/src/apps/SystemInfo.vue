<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue"
import { useI18n } from "vue-i18n"
import { Cpu, MemoryStick, RefreshCw, Server, Timer } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Progress } from "@/components/ui/progress"
import { Skeleton } from "@/components/ui/skeleton"
import type { SystemInfo } from "@/lib/api"
import { api, ApiError } from "@/lib/api"
import { formatBytes, formatNumber, splitDuration } from "@/lib/format"

const { t, locale } = useI18n()
const info = ref<SystemInfo | null>(null)
const error = ref<string | null>(null)
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    info.value = await api.systemInfo()
    error.value = null
  } catch (e) {
    error.value = e instanceof ApiError && e.code === "agent_unavailable" ? t("shell.agentDown") : t("common.error")
  } finally {
    loading.value = false
  }
}

let timer: number | undefined
onMounted(() => {
  load()
  timer = window.setInterval(load, 10_000)
})
onUnmounted(() => window.clearInterval(timer))

const memUsedPct = computed(() => {
  const m = info.value?.memory
  if (!m?.total_bytes) return 0
  return Math.round(((m.total_bytes - m.available_bytes) / m.total_bytes) * 100)
})

const uptime = computed(() => {
  if (!info.value) return ""
  const d = splitDuration(info.value.uptime_seconds)
  const parts: string[] = []
  if (d.days) parts.push(t("systemInfo.days", { n: d.days }))
  if (d.hours || d.days) parts.push(t("systemInfo.hours", { n: d.hours }))
  parts.push(t("systemInfo.minutes", { n: d.minutes }))
  return parts.join(" ")
})

const details = computed(() =>
  info.value
    ? [
        { label: t("systemInfo.hostname"), value: info.value.hostname },
        { label: t("systemInfo.os"), value: info.value.os || "—" },
        { label: t("systemInfo.kernel"), value: info.value.kernel || "—" },
        { label: t("systemInfo.arch"), value: info.value.arch },
        { label: t("systemInfo.version"), value: info.value.sapo_version },
      ]
    : [],
)
</script>

<template>
  <div class="space-y-4 p-4">
    <div v-if="error" class="flex items-center justify-between gap-3 rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm">
      <span>{{ error }}</span>
      <Button size="sm" variant="outline" @click="load">{{ t("common.retry") }}</Button>
    </div>

    <template v-if="info">
      <div class="grid gap-3 sm:grid-cols-2">
        <Card class="gap-3 py-4">
          <CardHeader class="px-4">
            <CardDescription class="flex items-center gap-2"><Cpu class="size-4" />{{ t("systemInfo.cpu") }}</CardDescription>
            <CardTitle class="text-base leading-snug">{{ info.cpu.model || info.arch }}</CardTitle>
          </CardHeader>
          <CardContent class="px-4 text-sm text-muted-foreground">
            {{ t("systemInfo.cores", { n: info.cpu.cores }) }}
          </CardContent>
        </Card>
        <Card class="gap-3 py-4">
          <CardHeader class="px-4">
            <CardDescription class="flex items-center gap-2"><MemoryStick class="size-4" />{{ t("systemInfo.memory") }}</CardDescription>
            <CardTitle class="text-base">{{ memUsedPct }}%</CardTitle>
          </CardHeader>
          <CardContent class="space-y-2 px-4">
            <Progress :model-value="memUsedPct" />
            <p class="text-xs text-muted-foreground">
              {{
                t("systemInfo.memoryUsed", {
                  used: formatBytes(info.memory.total_bytes - info.memory.available_bytes, locale),
                  total: formatBytes(info.memory.total_bytes, locale),
                })
              }}
            </p>
          </CardContent>
        </Card>
        <Card class="gap-3 py-4">
          <CardHeader class="px-4">
            <CardDescription class="flex items-center gap-2"><Timer class="size-4" />{{ t("systemInfo.uptime") }}</CardDescription>
            <CardTitle class="text-base">{{ uptime }}</CardTitle>
          </CardHeader>
        </Card>
        <Card class="gap-3 py-4">
          <CardHeader class="px-4">
            <CardDescription class="flex items-center gap-2"><Server class="size-4" />{{ t("systemInfo.load") }}</CardDescription>
            <CardTitle class="text-base tabular-nums">
              {{ (info.load_average ?? []).map((v) => formatNumber(v, locale)).join(" · ") || "—" }}
            </CardTitle>
          </CardHeader>
        </Card>
      </div>

      <Card class="py-2">
        <CardContent class="px-4">
          <dl class="divide-y text-sm">
            <div v-for="d in details" :key="d.label" class="flex items-center justify-between gap-4 py-2.5">
              <dt class="text-muted-foreground">{{ d.label }}</dt>
              <dd class="text-right font-medium break-all">{{ d.value }}</dd>
            </div>
          </dl>
        </CardContent>
      </Card>

      <div class="flex justify-end">
        <Button size="sm" variant="ghost" :disabled="loading" @click="load">
          <RefreshCw :class="loading && 'animate-spin'" />{{ t("systemInfo.refresh") }}
        </Button>
      </div>
    </template>

    <div v-else-if="!error" class="grid gap-3 sm:grid-cols-2">
      <Skeleton v-for="i in 4" :key="i" class="h-28 rounded-xl" />
    </div>
  </div>
</template>
