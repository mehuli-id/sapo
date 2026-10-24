<script setup lang="ts">
// Dibuka dari tautan konfirmasi setelah pengaturan jaringan diubah (ADR
// 0009): bila halaman ini terbuka di alamat baru, berarti panel bisa dibuka
// di sana, jadi pengaturan baru dikonfirmasi. Token diambil dari bagian #
// alamat (tidak dikirim ke server sebagai bagian URL).
import { onMounted, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, LoaderCircle, TriangleAlert } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { api, ApiError } from "@/lib/api"
import AuthLayout from "./AuthLayout.vue"

const props = defineProps<{ token: string }>()
const { t } = useI18n()
const state = ref<"working" | "ok" | "failed">("working")

onMounted(async () => {
  try {
    await api.confirmNetwork(props.token)
    state.value = "ok"
  } catch (e) {
    state.value = e instanceof ApiError && e.code === "busy" ? "working" : "failed"
    if (state.value === "working") setTimeout(() => window.location.reload(), 3000)
  }
})

function openPanel() {
  window.location.replace(window.location.pathname)
}
</script>

<template>
  <AuthLayout>
    <div class="space-y-4 text-sm">
      <p v-if="state === 'working'" class="flex items-center gap-2"><LoaderCircle class="size-4 animate-spin" />{{ t("network.confirmPage.working") }}</p>
      <p v-else-if="state === 'ok'" class="flex items-center gap-2"><CircleCheck class="size-4 text-emerald-600" />{{ t("network.confirmPage.ok") }}</p>
      <p v-else class="flex items-center gap-2"><TriangleAlert class="size-4 text-amber-600" />{{ t("network.confirmPage.failed") }}</p>
      <Button class="w-full" @click="openPanel">{{ t("network.confirmPage.open") }}</Button>
    </div>
  </AuthLayout>
</template>
