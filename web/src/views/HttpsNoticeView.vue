<script setup lang="ts">
// Ditampilkan saat panel dibuka lewat HTTP sementara HTTPS wajib (ADR 0004).
// Password tidak pernah dikirim dari halaman ini; pengguna diarahkan ke HTTPS
// setelah tahu peringatan sertifikat yang akan muncul dan cara memeriksanya.
import { computed } from "vue"
import { useI18n } from "vue-i18n"
import { ArrowRight, ShieldCheck } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import type { HTTPSConfig } from "@/lib/api"
import AuthLayout from "./AuthLayout.vue"

const props = defineProps<{ https: HTTPSConfig }>()
const { t } = useI18n()

// Nama host yang diketik pengguna dipertahankan (IP, nama.local, dll.).
const target = computed(() => {
  const u = new URL(window.location.href)
  u.protocol = "https:"
  u.port = props.https.port
  return u.toString()
})

// Sidik jari 32 byte ditampilkan dalam dua baris agar mudah dicocokkan.
const fingerprintLines = computed(() => {
  const parts = props.https.fingerprint_sha256.split(":")
  const half = Math.ceil(parts.length / 2)
  return [parts.slice(0, half).join(":"), parts.slice(half).join(":")]
})
</script>

<template>
  <AuthLayout>
    <div class="space-y-5">
      <div class="space-y-2">
        <h1 class="flex items-center gap-2 text-xl font-semibold"><ShieldCheck class="size-5 text-primary" />{{ t("https.title") }}</h1>
        <p class="text-sm text-muted-foreground">{{ t("https.intro") }}</p>
      </div>

      <ol class="list-decimal space-y-2 pl-5 text-sm">
        <li>{{ t("https.step1") }}</li>
        <li>{{ t("https.step2") }}</li>
        <li>{{ t("https.step3") }}</li>
      </ol>

      <div class="space-y-1.5 rounded-lg bg-muted p-3">
        <div class="text-xs font-medium text-muted-foreground">{{ t("https.fingerprint") }}</div>
        <code class="block font-mono text-[11px] leading-relaxed break-all">
          <span v-for="(line, i) in fingerprintLines" :key="i" class="block">{{ line }}</span>
        </code>
      </div>

      <Button as="a" :href="target" class="w-full">
        {{ t("https.open") }}<ArrowRight />
      </Button>
      <p class="text-center text-xs text-muted-foreground">{{ t("https.why") }}</p>
    </div>
  </AuthLayout>
</template>
