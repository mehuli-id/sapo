<script setup lang="ts">
// Tata letak halaman sebelum login (setup awal dan login).
import SapoLogo from "@/components/SapoLogo.vue"
import { useShell } from "@/composables/useShell"

defineProps<{ wide?: boolean }>()
const { branding } = useShell()
</script>

<template>
  <div class="desktop-wallpaper flex min-h-full items-center justify-center overflow-y-auto p-4 sm:p-8">
    <div :class="['w-full', wide ? 'max-w-xl' : 'max-w-sm']">
      <div class="mb-6 flex items-center justify-center gap-2 text-white">
        <SapoLogo :size="30" />
        <span class="text-lg font-semibold [text-shadow:0_1px_2px_rgb(0_0_0/0.4)]">{{ branding?.product.full_name ?? "Sapo" }}</span>
      </div>
      <div class="rounded-2xl border bg-card p-6 text-card-foreground shadow-2xl sm:p-8">
        <slot />
      </div>
    </div>
  </div>
</template>
