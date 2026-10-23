<script setup lang="ts">
import { onMounted, ref } from "vue"
import { TooltipProvider } from "@/components/ui/tooltip"
import { useSession } from "@/composables/useSession"
import { initMode, useShell } from "@/composables/useShell"
import CompactShell from "@/desktop/CompactShell.vue"
import DesktopShell from "@/desktop/DesktopShell.vue"
import { setLocale } from "@/i18n"
import { api } from "@/lib/api"
import { applyBranding } from "@/lib/theme"
import LoginView from "@/views/LoginView.vue"
import SetupView from "@/views/SetupView.vue"

const { effectiveMode, branding } = useShell()
const { status, policy, refresh } = useSession()
const ready = ref(false)

onMounted(async () => {
  try {
    const cfg = await api.uiConfig()
    initMode(cfg.default_mode)
    branding.value = cfg.branding
    policy.value = cfg.auth_policy
    applyBranding(cfg.branding)
    // Bahasa default dari server hanya dipakai bila pengguna belum memilih.
    let saved: string | null = null
    try {
      saved = localStorage.getItem("sapo.locale")
    } catch {
      // abaikan
    }
    if (!saved && cfg.default_language) setLocale(cfg.default_language)
  } catch {
    initMode("desktop")
  }
  await refresh()
  ready.value = true
})
</script>

<template>
  <TooltipProvider>
    <div v-if="!ready" class="desktop-wallpaper h-full" />
    <SetupView v-else-if="status?.setup_required" />
    <LoginView v-else-if="!status?.authenticated" />
    <DesktopShell v-else-if="effectiveMode === 'desktop'" />
    <CompactShell v-else />
  </TooltipProvider>
</template>
