<script setup lang="ts">
// Keamanan akun: status 2FA, kode cadangan, dan pindah aplikasi autentikator
// (misalnya HP baru). Setiap perubahan meminta password lagi.
import { computed, nextTick, ref } from "vue"
import { useI18n } from "vue-i18n"
import QRCode from "qrcode"
import { KeyRound, LoaderCircle, ShieldCheck, Smartphone, TriangleAlert } from "@lucide/vue"
import RecoveryCodes from "@/components/RecoveryCodes.vue"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useSession } from "@/composables/useSession"
import type { TOTPEnrollment } from "@/lib/api"
import { api, ApiError } from "@/lib/api"

const { t } = useI18n()
const { status, refresh } = useSession()

type Mode = "idle" | "codes-password" | "codes-shown" | "totp-password" | "totp-scan" | "totp-done"
const mode = ref<Mode>("idle")
const password = ref("")
const code = ref("")
const busy = ref(false)
const error = ref("")
const newCodes = ref<string[]>([])
const enrollment = ref<TOTPEnrollment | null>(null)
const qr = ref("")

const left = computed(() => status.value?.user?.recovery_codes_left)
const low = computed(() => status.value?.user?.recovery_codes_low)

// Fokus ke isian berikutnya. Atribut autofocus tidak berlaku untuk isian
// yang muncul belakangan di dalam jendela aplikasi.
async function focus(id: string) {
  await nextTick()
  document.getElementById(id)?.focus()
}

function start(m: Mode) {
  mode.value = m
  password.value = ""
  code.value = ""
  error.value = ""
  if (m === "codes-password") focus("acc-pass-codes")
  if (m === "totp-password") focus("acc-pass-totp")
}

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  if (e instanceof ApiError && c === "locked") {
    return t("errors.locked", { minutes: Math.max(1, Math.ceil(Number(e.detail.retry_after_seconds ?? 60) / 60)) })
  }
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`) : t("errors.generic")
}

async function run(fn: () => Promise<void>) {
  busy.value = true
  error.value = ""
  try {
    await fn()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

const regenerate = () =>
  run(async () => {
    const res = await api.regenerateRecoveryCodes(password.value)
    newCodes.value = res.recovery_codes
    password.value = ""
    mode.value = "codes-shown"
    await refresh()
  })

const beginTOTP = () =>
  run(async () => {
    const res = await api.totpBegin(password.value)
    enrollment.value = res
    qr.value = await QRCode.toDataURL(res.uri, { margin: 1, width: 192, errorCorrectionLevel: "M" })
    password.value = ""
    mode.value = "totp-scan"
    focus("acc-totp-code")
  })

const confirmTOTP = () =>
  run(async () => {
    await api.totpConfirm(enrollment.value!.token, code.value)
    enrollment.value = null
    qr.value = ""
    mode.value = "totp-done"
  })

const grouped = (s: string) => s.replace(/(.{4})/g, "$1 ").trim()
</script>

<template>
  <div class="space-y-4 p-4">
    <!-- Status -->
    <div class="flex items-start gap-3 rounded-xl border p-4">
      <ShieldCheck class="mt-0.5 size-5 shrink-0 text-primary" />
      <div class="space-y-1 text-sm">
        <div class="font-medium">{{ t("account.twofaOn") }}</div>
        <p v-if="left !== undefined" class="text-muted-foreground">{{ t("account.codesLeft", { n: left }) }}</p>
      </div>
    </div>
    <Alert v-if="low && mode !== 'codes-shown'" class="border-amber-500/50">
      <TriangleAlert class="text-amber-600" />
      <AlertDescription>{{ left === 0 ? t("account.codesNone") : t("account.codesLow", { n: left }) }}</AlertDescription>
    </Alert>

    <!-- Kode cadangan -->
    <section class="space-y-3 rounded-xl border p-4">
      <h3 class="flex items-center gap-2 font-medium"><KeyRound class="size-4" />{{ t("account.codesTitle") }}</h3>
      <p class="text-sm text-muted-foreground">{{ t("account.codesIntro") }}</p>
      <template v-if="mode === 'codes-password'">
        <form class="space-y-2" @submit.prevent="regenerate">
          <Label for="acc-pass-codes">{{ t("account.password") }}</Label>
          <Input id="acc-pass-codes" v-model="password" type="password" autocomplete="current-password" />
          <p class="text-xs text-muted-foreground">{{ t("account.codesWarn") }}</p>
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="busy || !password">
              <LoaderCircle v-if="busy" class="animate-spin" />{{ t("account.codesCreate") }}
            </Button>
            <Button type="button" size="sm" variant="ghost" @click="start('idle')">{{ t("account.cancel") }}</Button>
          </div>
        </form>
      </template>
      <template v-else-if="mode === 'codes-shown'">
        <p class="text-sm font-medium">{{ t("account.codesShownOnce") }}</p>
        <RecoveryCodes :codes="newCodes" :username="status?.user?.username" />
        <Button size="sm" variant="ghost" @click="start('idle')">{{ t("account.done") }}</Button>
      </template>
      <Button v-else size="sm" variant="outline" @click="start('codes-password')">{{ t("account.codesCreate") }}</Button>
    </section>

    <!-- Pindah aplikasi autentikator -->
    <section class="space-y-3 rounded-xl border p-4">
      <h3 class="flex items-center gap-2 font-medium"><Smartphone class="size-4" />{{ t("account.totpTitle") }}</h3>
      <p class="text-sm text-muted-foreground">{{ t("account.totpIntro") }}</p>
      <template v-if="mode === 'totp-password'">
        <form class="space-y-2" @submit.prevent="beginTOTP">
          <Label for="acc-pass-totp">{{ t("account.password") }}</Label>
          <Input id="acc-pass-totp" v-model="password" type="password" autocomplete="current-password" />
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="busy || !password">
              <LoaderCircle v-if="busy" class="animate-spin" />{{ t("account.continue") }}
            </Button>
            <Button type="button" size="sm" variant="ghost" @click="start('idle')">{{ t("account.cancel") }}</Button>
          </div>
        </form>
      </template>
      <template v-else-if="mode === 'totp-scan' && enrollment">
        <form class="space-y-3" @submit.prevent="confirmTOTP">
          <p class="text-sm">{{ t("account.totpScan") }}</p>
          <img v-if="qr" :src="qr" :alt="t('account.totpQr')" class="size-48 rounded-lg border bg-white p-1" />
          <code class="block rounded bg-muted px-2 py-1 font-mono text-xs break-all select-all">{{ grouped(enrollment.secret) }}</code>
          <Label for="acc-totp-code">{{ t("account.totpCode") }}</Label>
          <Input
            id="acc-totp-code"
            v-model="code"
            inputmode="numeric"
            autocomplete="one-time-code"
            maxlength="6"
            placeholder="123456"
            class="w-40 font-mono tracking-[0.3em]"
            @update:model-value="(v) => (code = String(v).replace(/\D/g, '').slice(0, 6))"
          />
          <p class="text-xs text-muted-foreground">{{ t("account.totpOldStillWorks") }}</p>
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="busy || code.length !== 6">
              <LoaderCircle v-if="busy" class="animate-spin" />{{ t("account.totpConfirm") }}
            </Button>
            <Button type="button" size="sm" variant="ghost" @click="start('idle')">{{ t("account.cancel") }}</Button>
          </div>
        </form>
      </template>
      <template v-else-if="mode === 'totp-done'">
        <p class="text-sm font-medium text-primary">{{ t("account.totpDone") }}</p>
        <Button size="sm" variant="ghost" @click="start('idle')">{{ t("account.done") }}</Button>
      </template>
      <Button v-else size="sm" variant="outline" @click="start('totp-password')">{{ t("account.totpStart") }}</Button>
    </section>

    <Alert v-if="error" variant="destructive">
      <AlertDescription>{{ error }}</AlertDescription>
    </Alert>
  </div>
</template>
