<script setup lang="ts">
// Pendaftaran 2FA: minta rahasia baru ke server, tampilkan QR, lalu terima kode.
import { onMounted, ref } from "vue"
import { useI18n } from "vue-i18n"
import QRCode from "qrcode"
import { Check, Copy, RefreshCw } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { api } from "@/lib/api"

// Hasil pendaftaran disimpan di state milik wizard, sehingga bolak-balik
// antarlangkah tidak membuat rahasia baru (yang akan membuat QR yang sudah
// dipindai tidak berlaku).
export interface Enrollment {
  token: string
  secret: string
  qr: string
  username: string
}

const props = defineProps<{ username: string; inputId: string; invalid?: boolean; enrollment: Enrollment }>()
const code = defineModel<string>({ default: "" })
const { t } = useI18n()

const failed = ref(false)
const copied = ref(false)

async function enroll() {
  failed.value = false
  props.enrollment.qr = ""
  props.enrollment.secret = ""
  try {
    const res = await api.setupTOTP(props.username)
    props.enrollment.token = res.token
    props.enrollment.secret = res.secret
    props.enrollment.username = props.username
    props.enrollment.qr = await QRCode.toDataURL(res.uri, { margin: 1, width: 192, errorCorrectionLevel: "M" })
    code.value = ""
  } catch {
    failed.value = true
  }
}

async function copy() {
  try {
    await navigator.clipboard.writeText(props.enrollment.secret)
    copied.value = true
    setTimeout(() => (copied.value = false), 2000)
  } catch {
    // clipboard tidak tersedia (misalnya bukan HTTPS); pengguna bisa menyalin manual
  }
}

// Kunci rahasia ditampilkan per 4 karakter agar mudah diketik manual.
const grouped = (s: string) => s.replace(/(.{4})/g, "$1 ").trim()

onMounted(() => {
  if (!props.enrollment.secret || props.enrollment.username !== props.username) enroll()
})
defineExpose({ enroll })
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-col items-center gap-3 rounded-xl border bg-muted/30 p-4 sm:flex-row sm:items-start">
      <div class="grid size-48 shrink-0 place-items-center rounded-lg bg-white p-2">
        <img v-if="enrollment.qr" :src="enrollment.qr" :alt="t('wizard.scanQr')" class="size-full" />
        <Skeleton v-else-if="!failed" class="size-full" />
        <Button v-else size="sm" variant="outline" @click="enroll"><RefreshCw />{{ t("common.retry") }}</Button>
      </div>
      <div class="min-w-0 space-y-2 text-sm">
        <p>{{ t("wizard.scanQr") }}</p>
        <p class="text-xs text-muted-foreground">{{ t("wizard.secretKey") }}</p>
        <div class="flex items-center gap-2">
          <code class="rounded bg-background px-2 py-1 font-mono text-xs break-all select-all">{{ grouped(enrollment.secret) }}</code>
          <Button v-if="enrollment.secret" size="icon-sm" variant="ghost" :aria-label="t('wizard.copy')" @click="copy">
            <Check v-if="copied" class="text-primary" />
            <Copy v-else />
          </Button>
        </div>
      </div>
    </div>
    <div class="space-y-1.5">
      <Input
        :id="inputId"
        v-model="code"
        inputmode="numeric"
        autocomplete="one-time-code"
        maxlength="6"
        placeholder="123456"
        class="max-w-40 text-center font-mono text-lg tracking-[0.3em]"
        :aria-invalid="invalid || undefined"
        @update:model-value="(v) => (code = String(v).replace(/\D/g, '').slice(0, 6))"
      />
      <p class="text-xs text-muted-foreground">{{ t("wizard.codeHint") }}</p>
    </div>
  </div>
</template>
