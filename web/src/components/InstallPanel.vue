<script setup lang="ts">
// Installer ke disk (ADR 0016): tampil hanya saat Sapo berjalan dari ISO.
// Tombol membuka wizard (wizards/install.yaml); kemajuan dibaca berkala
// selama pemasangan berjalan.
import { onMounted, onUnmounted, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, HardDriveDownload, LoaderCircle, TriangleAlert } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import type { InstallStatus } from "@/lib/api"
import { api } from "@/lib/api"

defineEmits<{ install: [] }>()
const { t } = useI18n()
const status = ref<InstallStatus | null>(null)
let timer: number | undefined

async function load() {
  try {
    status.value = await api.installStatus()
  } catch {
    status.value = null
  }
  window.clearTimeout(timer)
  if (status.value?.running) timer = window.setTimeout(load, 3000)
}
onMounted(load)
onUnmounted(() => window.clearTimeout(timer))
defineExpose({ load })
</script>

<template>
  <section v-if="status?.live" class="space-y-2 rounded-xl border border-primary/40 bg-primary/5 p-4">
    <h3 class="flex items-center gap-2 text-sm font-semibold"><HardDriveDownload class="size-4" />{{ t("install.title") }}</h3>
    <template v-if="status.running">
      <p class="flex items-center gap-2 text-sm"><LoaderCircle class="size-4 animate-spin" />{{ t("install.running", { step: status.last?.step ?? "" }) }}</p>
      <pre class="max-h-40 overflow-y-auto rounded bg-muted p-2 text-xs whitespace-pre-wrap">{{ status.log.slice(-8).join("\n") }}</pre>
    </template>
    <p v-else-if="status.last?.ok" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ t("install.done", { disk: status.last.disk }) }}</p>
    <template v-else>
      <p v-if="status.last && !status.last.ok" class="flex items-center gap-2 text-sm text-destructive" role="alert">
        <TriangleAlert class="size-4" />{{ t("install.failed", { step: status.last.step, error: status.last.error || (status.last.interrupted ? t("install.interrupted") : "") }) }}
      </p>
      <p class="text-sm">{{ t("install.help") }}</p>
      <Button size="sm" @click="$emit('install')">{{ t("install.start") }}</Button>
    </template>
  </section>
</template>
