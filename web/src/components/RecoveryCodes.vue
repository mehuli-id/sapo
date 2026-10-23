<script setup lang="ts">
// Menampilkan kode cadangan 2FA (sekali, saat baru dibuat) dengan tombol
// salin dan unduh. Kode tidak pernah disimpan di browser.
import { ref } from "vue"
import { useI18n } from "vue-i18n"
import { Check, Copy, Download } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { useShell } from "@/composables/useShell"

const props = defineProps<{ codes: string[]; username?: string }>()
const { t } = useI18n()
const { branding } = useShell()
const copied = ref(false)

function asText() {
  const product = branding.value?.product.full_name ?? "Sapo"
  const host = window.location.hostname
  const who = props.username ? ` (${props.username})` : ""
  return [t("recovery.fileTitle", { product, host }) + who, t("recovery.fileHint"), "", ...props.codes, ""].join("\n")
}

async function copy() {
  try {
    await navigator.clipboard.writeText(asText())
    copied.value = true
    setTimeout(() => (copied.value = false), 2000)
  } catch {
    // clipboard tidak tersedia; pengguna bisa memakai Unduh
  }
}

function download() {
  const url = URL.createObjectURL(new Blob([asText()], { type: "text/plain" }))
  const a = document.createElement("a")
  a.href = url
  a.download = `kode-cadangan-${window.location.hostname}.txt`
  a.click()
  URL.revokeObjectURL(url)
}
</script>

<template>
  <div class="space-y-3">
    <ol class="grid grid-cols-2 gap-x-6 gap-y-1.5 rounded-lg bg-muted p-3 font-mono text-sm tabular-nums">
      <li v-for="(c, i) in codes" :key="c" class="flex gap-2">
        <span class="w-5 text-right text-muted-foreground select-none">{{ i + 1 }}.</span><span class="select-all">{{ c }}</span>
      </li>
    </ol>
    <div class="flex flex-wrap gap-2">
      <Button type="button" size="sm" variant="outline" @click="download"><Download />{{ t("recovery.download") }}</Button>
      <Button type="button" size="sm" variant="outline" @click="copy">
        <Check v-if="copied" /><Copy v-else />{{ copied ? t("recovery.copied") : t("recovery.copy") }}
      </Button>
    </div>
  </div>
</template>
