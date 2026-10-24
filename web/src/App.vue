<script setup lang="ts">
import { onMounted, ref, watch } from "vue"
import { TooltipProvider } from "@/components/ui/tooltip"
import { useSession } from "@/composables/useSession"
import { initMode, useShell } from "@/composables/useShell"
import { useWindows } from "@/composables/useWindows"
import CompactShell from "@/desktop/CompactShell.vue"
import DesktopShell from "@/desktop/DesktopShell.vue"
import { setLocale } from "@/i18n"
import type { HTTPSConfig } from "@/lib/api"
import { api } from "@/lib/api"
import { applyBranding } from "@/lib/theme"
import HttpsNoticeView from "@/views/HttpsNoticeView.vue"
import LoginView from "@/views/LoginView.vue"
import NetworkConfirmView from "@/views/NetworkConfirmView.vue"
import AccountSetupView from "@/views/AccountSetupView.vue"
import SetupView from "@/views/SetupView.vue"

const { effectiveMode, branding } = useShell()
const { status, policy, refresh } = useSession()
const ready = ref(false)
// Jendela yang terbuka milik sesi user sebelumnya ditutup saat user berganti
// atau keluar.
const { closeAll } = useWindows()
watch(
  () => status.value?.user?.username,
  (now, before) => {
    if (now !== before) closeAll()
  },
)
// Tautan konfirmasi pengaturan jaringan (#network-confirm=<token>).
const networkToken = /^#network-confirm=([0-9a-f]{64})$/.exec(window.location.hash)?.[1] ?? ""
// Diisi bila panel dibuka lewat HTTP padahal HTTPS wajib.
const httpsNotice = ref<HTTPSConfig | null>(null)

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
    if (cfg.https && window.location.protocol === "http:") {
      httpsNotice.value = cfg.https
      ready.value = true
      return
    }
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
    <HttpsNoticeView v-else-if="httpsNotice" :https="httpsNotice" />
    <NetworkConfirmView v-else-if="networkToken" :token="networkToken" />
    <SetupView v-else-if="status?.setup_required" />
    <LoginView v-else-if="!status?.authenticated" />
    <AccountSetupView v-else-if="status.user?.pending?.length" />
    <DesktopShell v-else-if="effectiveMode === 'desktop'" />
    <CompactShell v-else />
  </TooltipProvider>
</template>
