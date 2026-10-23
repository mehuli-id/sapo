<script setup lang="ts">
import { computed, nextTick, ref } from "vue"
import { useI18n } from "vue-i18n"
import { ArrowLeft, LoaderCircle } from "@lucide/vue"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useSession } from "@/composables/useSession"
import { useShell } from "@/composables/useShell"
import { api, ApiError } from "@/lib/api"
import AuthLayout from "./AuthLayout.vue"

const { t } = useI18n()
const { refresh } = useSession()
const { branding } = useShell()

const username = ref("")
const password = ref("")
const code = ref("")
const needCode = ref(false)
const busy = ref(false)
const error = ref("")
const codeInput = ref<InstanceType<typeof Input> | null>(null)

async function submit() {
  error.value = ""
  if (!username.value || !password.value) return
  busy.value = true
  try {
    await api.login(username.value, password.value, needCode.value ? code.value : undefined)
    password.value = ""
    code.value = ""
    await refresh()
  } catch (e) {
    if (e instanceof ApiError && e.code === "locked_need_code") {
      // Akun sedang dilindungi: password dan kode dikirim sekaligus.
      needCode.value = true
      error.value = t("errors.locked_need_code")
      await nextTick()
      ;(codeInput.value?.$el as HTMLInputElement | undefined)?.focus()
    } else if (e instanceof ApiError && e.code === "totp_required") {
      needCode.value = true
      await nextTick()
      ;(codeInput.value?.$el as HTMLInputElement | undefined)?.focus()
    } else if (e instanceof ApiError && e.code === "locked") {
      const minutes = Math.max(1, Math.ceil(Number(e.detail.retry_after_seconds ?? 60) / 60))
      error.value = t("errors.locked", { minutes })
    } else {
      const c = e instanceof ApiError ? e.code : "generic"
      error.value = t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`) : t("errors.generic")
      if (c === "totp_invalid") code.value = ""
    }
  } finally {
    busy.value = false
  }
}

function backToPassword() {
  needCode.value = false
  useRecovery.value = false
  code.value = ""
  error.value = ""
}

// Kode cadangan (HP autentikator tidak tersedia): XXXXX-XXXXX, 10 karakter
// huruf/angka. Dikirim di kolom yang sama dengan kode 2FA.
const useRecovery = ref(false)
const codeReady = computed(() =>
  useRecovery.value ? code.value.replace(/[^0-9a-z]/gi, "").length === 10 : code.value.length === 6,
)

async function toggleRecovery() {
  useRecovery.value = !useRecovery.value
  code.value = ""
  error.value = ""
  await nextTick()
  ;(codeInput.value?.$el as HTMLInputElement | undefined)?.focus()
}
</script>

<template>
  <AuthLayout>
    <form class="space-y-5" novalidate @submit.prevent="submit">
      <div class="space-y-1">
        <h1 class="text-xl font-semibold">{{ t("login.title", { name: branding?.product.name ?? "Sapo" }) }}</h1>
        <p class="text-sm text-muted-foreground">
          {{ needCode ? (useRecovery ? t("login.recoveryHint") : t("login.codeHint")) : t("login.subtitle") }}
        </p>
      </div>

      <template v-if="!needCode">
        <div class="space-y-1.5">
          <Label for="login-username">{{ t("login.username") }}</Label>
          <Input
            id="login-username"
            v-model="username"
            autocomplete="username"
            autocapitalize="off"
            spellcheck="false"
            autofocus
            @update:model-value="(v) => (username = String(v).toLowerCase())"
          />
        </div>
        <div class="space-y-1.5">
          <Label for="login-password">{{ t("login.password") }}</Label>
          <Input id="login-password" v-model="password" type="password" autocomplete="current-password" />
        </div>
      </template>

      <div v-else class="space-y-1.5">
        <Label for="login-code">{{ useRecovery ? t("login.recoveryCode") : t("login.code") }}</Label>
        <Input
          v-if="useRecovery"
          id="login-code"
          ref="codeInput"
          v-model="code"
          autocomplete="off"
          autocapitalize="characters"
          spellcheck="false"
          maxlength="11"
          placeholder="XXXXX-XXXXX"
          class="text-center font-mono text-lg tracking-widest uppercase"
          @update:model-value="(v) => (code = String(v).toUpperCase())"
        />
        <Input
          v-else
          id="login-code"
          ref="codeInput"
          v-model="code"
          inputmode="numeric"
          autocomplete="one-time-code"
          maxlength="6"
          placeholder="123456"
          class="text-center font-mono text-lg tracking-[0.3em]"
          @update:model-value="(v) => (code = String(v).replace(/\D/g, '').slice(0, 6))"
        />
        <button type="button" class="text-xs text-muted-foreground underline-offset-2 hover:underline" @click="toggleRecovery">
          {{ useRecovery ? t("login.useApp") : t("login.useRecovery") }}
        </button>
      </div>

      <Alert v-if="error" variant="destructive">
        <AlertDescription>{{ error }}</AlertDescription>
      </Alert>

      <div class="flex items-center gap-2">
        <Button v-if="needCode" type="button" variant="ghost" :disabled="busy" @click="backToPassword">
          <ArrowLeft />{{ t("login.back") }}
        </Button>
        <Button type="submit" class="flex-1" :disabled="busy || (needCode && !codeReady)">
          <template v-if="busy"><LoaderCircle class="animate-spin" />{{ t("login.submitting") }}</template>
          <template v-else>{{ t("login.submit") }}</template>
        </Button>
      </div>
    </form>
  </AuthLayout>
</template>
