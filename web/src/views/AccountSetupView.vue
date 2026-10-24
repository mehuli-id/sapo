<script setup lang="ts">
// Langkah wajib akun sebelum panel bisa dipakai (status.user.pending):
// mengganti password yang dibuat admin, lalu (untuk administrator) memasang
// verifikasi dua langkah beserta kode cadangan.
import { computed, ref } from "vue"
import { useI18n } from "vue-i18n"
import QRCode from "qrcode"
import { LoaderCircle, LogOut } from "@lucide/vue"
import RecoveryCodes from "@/components/RecoveryCodes.vue"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useSession } from "@/composables/useSession"
import type { TOTPEnrollment } from "@/lib/api"
import { api, ApiError } from "@/lib/api"
import AuthLayout from "./AuthLayout.vue"

const { t } = useI18n()
const { status, policy, refresh, logout } = useSession()
const step = computed(() => (status.value?.user?.pending ?? [])[0])
const username = computed(() => status.value?.user?.username ?? "")

const busy = ref(false)
const error = ref("")
const current = ref("")
const next = ref("")
const confirm = ref("")
const code = ref("")
const enrollment = ref<TOTPEnrollment | null>(null)
const qr = ref("")
const codes = ref<string[]>([])
const saved = ref(false)

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
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

const changePassword = () =>
  run(async () => {
    await api.changePassword(current.value, next.value, confirm.value)
    // Password baru dipakai lagi bila langkah 2FA menyusul.
    current.value = next.value
    next.value = confirm.value = ""
    await refresh()
  })

const beginTOTP = () =>
  run(async () => {
    const res = await api.totpBegin(current.value)
    enrollment.value = res
    qr.value = await QRCode.toDataURL(res.uri, { margin: 1, width: 192, errorCorrectionLevel: "M" })
  })

const confirmTOTP = () =>
  run(async () => {
    await api.totpConfirm(enrollment.value!.token, code.value)
    codes.value = (await api.regenerateRecoveryCodes(current.value)).recovery_codes
    current.value = ""
    enrollment.value = null
  })

const finish = () => run(refresh)
const grouped = (s: string) => s.replace(/(.{4})/g, "$1 ").trim()
</script>

<template>
  <AuthLayout wide>
    <div class="space-y-5">
      <div class="flex items-start justify-between gap-3">
        <div class="space-y-1">
          <h1 class="text-xl font-semibold">{{ t("setupAccount.title", { name: username }) }}</h1>
          <p class="text-sm text-muted-foreground">{{ t("setupAccount.intro") }}</p>
        </div>
        <Button size="sm" variant="ghost" @click="logout"><LogOut />{{ t("shell.logout") }}</Button>
      </div>

      <!-- Ganti password -->
      <form v-if="step === 'password'" class="space-y-3" @submit.prevent="changePassword">
        <h2 class="font-medium">{{ t("setupAccount.passwordTitle") }}</h2>
        <p class="text-sm text-muted-foreground">{{ t("setupAccount.passwordIntro") }}</p>
        <div class="space-y-1.5">
          <Label for="as-current">{{ t("setupAccount.current") }}</Label>
          <Input id="as-current" v-model="current" type="password" autocomplete="current-password" />
        </div>
        <div class="space-y-1.5">
          <Label for="as-new">{{ t("users.newPassword") }}</Label>
          <Input id="as-new" v-model="next" type="password" autocomplete="new-password" />
          <p class="text-xs text-muted-foreground">{{ t("wizard.tooShort", { n: policy.password_min_length }) }}</p>
        </div>
        <div class="space-y-1.5">
          <Label for="as-confirm">{{ t("users.repeatPassword") }}</Label>
          <Input id="as-confirm" v-model="confirm" type="password" autocomplete="new-password" />
        </div>
        <Button type="submit" :disabled="busy || !current || !next || next !== confirm">
          <LoaderCircle v-if="busy" class="animate-spin" />{{ t("setupAccount.savePassword") }}
        </Button>
      </form>

      <!-- 2FA untuk administrator -->
      <template v-else-if="step === 'totp'">
        <h2 class="font-medium">{{ t("setupAccount.totpTitle") }}</h2>
        <p class="text-sm text-muted-foreground">{{ t("setupAccount.totpIntro") }}</p>

        <template v-if="codes.length">
          <p class="text-sm font-medium">{{ t("account.codesShownOnce") }}</p>
          <RecoveryCodes :codes="codes" :username="username" />
          <label class="flex items-start gap-2 text-sm">
            <input v-model="saved" type="checkbox" class="mt-0.5 size-4 accent-primary" />{{ t("setupAccount.codesSaved") }}
          </label>
          <Button :disabled="!saved || busy" @click="finish">{{ t("setupAccount.finish") }}</Button>
        </template>

        <form v-else-if="enrollment" class="space-y-3" @submit.prevent="confirmTOTP">
          <p class="text-sm">{{ t("wizard.scanQr") }}</p>
          <img v-if="qr" :src="qr" :alt="t('account.totpQr')" class="size-48 rounded-lg border bg-white p-1" />
          <code class="block rounded bg-muted px-2 py-1 font-mono text-xs break-all select-all">{{ grouped(enrollment.secret) }}</code>
          <Label for="as-code">{{ t("account.totpCode") }}</Label>
          <Input
            id="as-code"
            v-model="code"
            inputmode="numeric"
            autocomplete="one-time-code"
            maxlength="6"
            placeholder="123456"
            class="w-40 font-mono tracking-[0.3em]"
            @update:model-value="(v) => (code = String(v).replace(/\D/g, '').slice(0, 6))"
          />
          <Button type="submit" :disabled="busy || code.length !== 6"><LoaderCircle v-if="busy" class="animate-spin" />{{ t("account.totpConfirm") }}</Button>
        </form>

        <form v-else class="space-y-3" @submit.prevent="beginTOTP">
          <div class="space-y-1.5">
            <Label for="as-pass">{{ t("account.password") }}</Label>
            <Input id="as-pass" v-model="current" type="password" autocomplete="current-password" />
          </div>
          <Button type="submit" :disabled="busy || !current"><LoaderCircle v-if="busy" class="animate-spin" />{{ t("account.continue") }}</Button>
        </form>
      </template>

      <Alert v-if="error" variant="destructive">
        <AlertDescription>{{ error }}</AlertDescription>
      </Alert>
    </div>
  </AuthLayout>
</template>
