<script setup lang="ts">
// Renderer wizard generik. Langkah, isian, teks, dan petunjuk datang dari
// data (wizards/<id>.yaml lewat API); komponen ini hanya tahu cara
// menampilkan setiap jenis isian.
import { computed, reactive, ref, watch } from "vue"
import { useI18n } from "vue-i18n"
import { ArrowLeft, ArrowRight, Check, CircleHelp, Eye, EyeOff, LoaderCircle } from "@lucide/vue"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { languages, setLocale } from "@/i18n"
import type { Wizard, WizardField } from "@/lib/api"
import { ApiError } from "@/lib/api"
import { pick } from "@/lib/localized"
import { cn } from "@/lib/utils"
import type { Enrollment } from "./TotpEnroll.vue"
import TotpEnroll from "./TotpEnroll.vue"
import RecoveryCodes from "@/components/RecoveryCodes.vue"

const props = defineProps<{
  wizard: Wizard
  policy: { password_min_length: number; username_pattern: string }
  submit: (values: Record<string, string>) => Promise<unknown>
}>()
const emit = defineEmits<{ done: [] }>()

const { t, locale } = useI18n()
const index = ref(0)
const busy = ref(false)
const formError = ref("")
const showHelp = ref(false)
const reveal = reactive<Record<string, boolean>>({})
const errors = reactive<Record<string, string>>({})
const enrollment = reactive<Enrollment>({ token: "", secret: "", qr: "", username: "", recoveryCodes: [] })

// Kode cadangan baru (pendaftaran 2FA diulang, misalnya setelah nama
// pengguna diganti): konfirmasi "sudah disimpan" untuk kode lama tidak berlaku.
watch(
  () => enrollment.token,
  () => {
    for (const st of props.wizard.steps) for (const f of st.fields ?? []) if (f.type === "recovery-codes") values[f.key] = ""
  },
)

const values = reactive<Record<string, string>>({})
for (const st of props.wizard.steps) for (const f of st.fields ?? []) values[f.key] = f.default ?? ""
if ("language" in values) values.language = String(locale.value)

const step = computed(() => props.wizard.steps[index.value]!)
const isLast = computed(() => index.value === props.wizard.steps.length - 1)
const usernameRe = computed(() => new RegExp(props.policy.username_pattern))

function validateField(f: WizardField): string {
  const v = values[f.key] ?? ""
  // Kotak centang kode cadangan punya pesan sendiri (bukan "wajib diisi").
  if (f.type === "recovery-codes") return v === "yes" ? "" : t("recovery.mustConfirm")
  if (f.required && !v.trim()) return t("wizard.required")
  switch (f.type) {
    case "username":
      return usernameRe.value.test(v) ? "" : t("wizard.patternMismatch")
    case "password":
      return [...v].length >= props.policy.password_min_length ? "" : t("wizard.tooShort", { n: props.policy.password_min_length })
    case "password-confirm":
      return f.match && v !== values[f.match] ? t("wizard.mismatch") : ""
    case "totp-enroll":
      return /^\d{6}$/.test(v) ? "" : t("wizard.codeHint")
  }
  return ""
}

function validateStep(): boolean {
  let ok = true
  for (const f of step.value.fields ?? []) {
    errors[f.key] = validateField(f)
    if (errors[f.key]) ok = false
  }
  return ok
}

function next() {
  formError.value = ""
  if (!validateStep()) return
  if (!isLast.value) {
    index.value++
    showHelp.value = false
  } else {
    finish()
  }
}

function back() {
  formError.value = ""
  if (index.value > 0) index.value--
}

async function finish() {
  busy.value = true
  formError.value = ""
  try {
    // Token pendaftaran 2FA menghubungkan kode dengan QR yang dipindai di
    // browser ini. Konfirmasi "kode cadangan sudah disimpan" hanya untuk
    // wizard, tidak dikirim ke server.
    const payload: Record<string, string> = {}
    for (const st of props.wizard.steps)
      for (const f of st.fields ?? []) if (f.type !== "recovery-codes") payload[f.key] = values[f.key] ?? ""
    await props.submit(enrollment.token ? { ...payload, totp_token: enrollment.token } : payload)
    emit("done")
  } catch (e) {
    const code = e instanceof ApiError ? e.code : "generic"
    // Rahasia 2FA kedaluwarsa di server: daftar ulang saat langkah 2FA dibuka.
    if (code === "totp_expired") {
      // Pendaftaran baru = kode cadangan baru: konfirmasi lama tidak berlaku.
      enrollment.secret = ""
      for (const st of props.wizard.steps) for (const f of st.fields ?? []) if (f.type === "recovery-codes") values[f.key] = ""
    }
    const msg = t(`errors.${code}`) !== `errors.${code}` ? t(`errors.${code}`) : t("errors.generic")
    // Lompat ke langkah yang berisi isian bermasalah.
    const field = e instanceof ApiError ? String(e.detail.field ?? "") : ""
    const at = field ? props.wizard.steps.findIndex((s) => s.fields?.some((f) => f.key === field)) : -1
    if (at >= 0) {
      index.value = at
      errors[field] = msg
    } else {
      formError.value = msg
    }
  } finally {
    busy.value = false
  }
}

function onLanguage(code: string) {
  values.language = code
  setLocale(code)
}

const reviewRows = computed(() =>
  props.wizard.steps.flatMap((st) =>
    (st.fields ?? [])
      .filter((f) => f.type !== "password-confirm")
      .map((f) => {
        let value = values[f.key] ?? ""
        let label = pick(f.label, locale.value)
        if (f.type === "language") value = languages.find((l) => l.code === value)?.label ?? value
        else if (f.type === "totp-enroll") {
          // Yang relevan di ringkasan adalah statusnya, bukan kodenya.
          label = pick(st.title, locale.value)
          value = t("wizard.twofaOn")
        } else if (f.type === "recovery-codes") {
          label = pick(st.title, locale.value)
          value = t("recovery.saved", { n: enrollment.recoveryCodes.length })
        } else if (f.secret) value = t("wizard.hidden")
        return { key: f.key, label, value }
      }),
  ),
)
</script>

<template>
  <div class="flex flex-col gap-6">
    <!-- Progres -->
    <ol class="flex items-center gap-2" :aria-label="t('wizard.step', { n: index + 1, total: wizard.steps.length })">
      <li v-for="(s, i) in wizard.steps" :key="s.id" class="flex flex-1 items-center gap-2">
        <span
          :class="
            cn(
              'grid size-7 shrink-0 place-items-center rounded-full border text-xs font-medium transition-colors',
              i < index && 'border-primary bg-primary text-primary-foreground',
              i === index && 'border-primary text-primary',
              i > index && 'text-muted-foreground',
            )
          "
          :aria-current="i === index ? 'step' : undefined"
        >
          <Check v-if="i < index" class="size-3.5" />
          <template v-else>{{ i + 1 }}</template>
        </span>
        <span v-if="i < wizard.steps.length - 1" :class="cn('h-px flex-1', i < index ? 'bg-primary' : 'bg-border')" />
      </li>
    </ol>

    <div class="space-y-1.5">
      <p class="text-xs text-muted-foreground">{{ t("wizard.step", { n: index + 1, total: wizard.steps.length }) }}</p>
      <div class="flex items-start justify-between gap-3">
        <h2 class="text-xl font-semibold">{{ pick(step.title, locale) }}</h2>
        <Button v-if="step.help" size="sm" variant="ghost" :aria-expanded="showHelp" @click="showHelp = !showHelp">
          <CircleHelp />{{ t("wizard.help") }}
        </Button>
      </div>
      <p v-if="step.description" class="text-sm text-muted-foreground">{{ pick(step.description, locale) }}</p>
    </div>

    <Alert v-if="showHelp && step.help" class="bg-primary/5">
      <CircleHelp class="text-primary" />
      <AlertDescription class="text-foreground/90">{{ pick(step.help, locale) }}</AlertDescription>
    </Alert>

    <form class="space-y-5" novalidate @submit.prevent="next">
      <template v-if="step.type === 'review'">
        <dl class="divide-y rounded-xl border text-sm">
          <div v-for="row in reviewRows" :key="row.key" class="flex justify-between gap-4 px-4 py-3">
            <dt class="text-muted-foreground">{{ row.label }}</dt>
            <dd class="text-right font-medium break-all">{{ row.value }}</dd>
          </div>
        </dl>
      </template>

      <div v-for="f in step.fields ?? []" :key="f.key" class="space-y-1.5">
        <!-- Label kode cadangan ada di samping kotak centangnya. -->
        <Label v-if="f.type !== 'recovery-codes'" :for="`wz-${f.key}`">{{ pick(f.label, locale) }}</Label>

        <!-- Bahasa -->
        <div v-if="f.type === 'language'" :id="`wz-${f.key}`" role="radiogroup" class="grid gap-2 sm:grid-cols-2">
          <button
            v-for="l in languages"
            :key="l.code"
            type="button"
            role="radio"
            :aria-checked="values[f.key] === l.code"
            :class="
              cn(
                'flex items-center justify-between rounded-lg border px-4 py-3 text-left text-sm hover:bg-accent',
                values[f.key] === l.code && 'border-primary ring-1 ring-primary',
              )
            "
            @click="onLanguage(l.code)"
          >
            {{ l.label }}
            <Check v-if="values[f.key] === l.code" class="size-4 text-primary" />
          </button>
        </div>

        <!-- 2FA -->
        <TotpEnroll
          v-else-if="f.type === 'totp-enroll'"
          v-model="values[f.key]"
          :username="values.username ?? ''"
          :input-id="`wz-${f.key}`"
          :invalid="!!errors[f.key]"
          :enrollment="enrollment"
        />

        <!-- Kode cadangan 2FA: wajib dikonfirmasi sudah disimpan -->
        <div v-else-if="f.type === 'recovery-codes'" class="space-y-3">
          <RecoveryCodes v-if="enrollment.recoveryCodes.length" :codes="enrollment.recoveryCodes" :username="values.username" />
          <p v-else class="text-sm text-muted-foreground">{{ t("recovery.needTwofa") }}</p>
          <label class="flex items-start gap-2 text-sm">
            <input
              :id="`wz-${f.key}`"
              type="checkbox"
              class="mt-0.5 size-4 accent-primary"
              :checked="values[f.key] === 'yes'"
              :disabled="!enrollment.recoveryCodes.length"
              @change="values[f.key] = ($event.target as HTMLInputElement).checked ? 'yes' : ''"
            />
            <span>{{ pick(f.label, locale) }}</span>
          </label>
        </div>

        <!-- Password -->
        <div v-else-if="f.type === 'password' || f.type === 'password-confirm'" class="relative">
          <Input
            :id="`wz-${f.key}`"
            v-model="values[f.key]"
            :type="reveal[f.key] ? 'text' : 'password'"
            :autocomplete="f.autocomplete"
            :aria-invalid="!!errors[f.key] || undefined"
            class="pr-10"
          />
          <button
            type="button"
            class="absolute top-1/2 right-1 grid size-7 -translate-y-1/2 place-items-center rounded text-muted-foreground hover:text-foreground"
            :aria-label="reveal[f.key] ? t('wizard.hide') : t('wizard.show')"
            @click="reveal[f.key] = !reveal[f.key]"
          >
            <EyeOff v-if="reveal[f.key]" class="size-4" />
            <Eye v-else class="size-4" />
          </button>
        </div>

        <!-- Teks / nama pengguna -->
        <Input
          v-else
          :id="`wz-${f.key}`"
          v-model="values[f.key]"
          :autocomplete="f.autocomplete"
          :placeholder="pick(f.placeholder, locale)"
          :aria-invalid="!!errors[f.key] || undefined"
          autocapitalize="off"
          spellcheck="false"
          @update:model-value="(v) => f.type === 'username' && (values[f.key] = String(v).toLowerCase())"
        />

        <p v-if="errors[f.key]" class="text-xs text-destructive" role="alert">{{ errors[f.key] }}</p>
        <p v-else-if="f.type === 'password' && values[f.key]" class="text-xs text-muted-foreground">
          {{ t("wizard.strength", { n: [...(values[f.key] ?? '')].length }) }}
        </p>
        <p v-else-if="f.help" class="text-xs text-muted-foreground">{{ pick(f.help, locale) }}</p>
      </div>

      <Alert v-if="formError" variant="destructive">
        <AlertDescription>{{ formError }}</AlertDescription>
      </Alert>

      <div class="flex items-center justify-between gap-3 pt-2">
        <Button type="button" variant="ghost" :disabled="index === 0 || busy" @click="back">
          <ArrowLeft />{{ t("wizard.back") }}
        </Button>
        <Button type="submit" :disabled="busy">
          <template v-if="busy"><LoaderCircle class="animate-spin" />{{ t("wizard.finishing") }}</template>
          <template v-else-if="isLast"><Check />{{ t("wizard.finish") }}</template>
          <template v-else>{{ t("wizard.next") }}<ArrowRight /></template>
        </Button>
      </div>
    </form>
  </div>
</template>
