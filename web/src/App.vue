<script setup lang="ts">
import { onMounted } from "vue"
import { TooltipProvider } from "@/components/ui/tooltip"
import { initMode, useShell } from "@/composables/useShell"
import CompactShell from "@/desktop/CompactShell.vue"
import DesktopShell from "@/desktop/DesktopShell.vue"
import { api } from "@/lib/api"
import { applyBranding } from "@/lib/theme"

const { effectiveMode, branding } = useShell()

onMounted(async () => {
  try {
    const cfg = await api.uiConfig()
    initMode(cfg.default_mode)
    branding.value = cfg.branding
    applyBranding(cfg.branding)
  } catch {
    initMode("desktop")
  }
})
</script>

<template>
  <TooltipProvider>
    <DesktopShell v-if="effectiveMode === 'desktop'" />
    <CompactShell v-else />
  </TooltipProvider>
</template>
