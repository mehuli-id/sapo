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
import type { Disk, Wizard, WizardContext, WizardField } from "@/lib/api"
import { ApiError, formatPermissions, parsePermissions, splitList } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { pick } from "@/lib/localized"
import { cn } from "@/lib/utils"
import type { Enrollment } from "./TotpEnroll.vue"
import TotpEnroll from "./TotpEnroll.vue"
import PermissionEditor from "@/components/PermissionEditor.vue"
import RecoveryCodes from "@/components/RecoveryCodes.vue"
import { fits } from "@/lib/pool"
import DiskSelect from "./DiskSelect.vue"
import EraseConfirm from "./EraseConfirm.vue"
import PoolLayoutSelect from "./PoolLayoutSelect.vue"

const props = defineProps<{
  wizard: Wizard
  policy: { password_min_length: number; username_pattern: string }
  submit: (values: Record<string, string>) => Promise<unknown>
  // Data untuk isian tertentu (daftar disk, susunan pool), dari aplikasi pembuka.
  context?: WizardContext
  // Wizard di dalam aplikasi bisa dibatalkan dari langkah pertama.
  cancelable?: boolean
}>()
const emit = defineEmits<{ done: [values: Record<string, string>]; cancel: [] }>()

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

// Pesan error isian hilang begitu isiannya diubah.
watch(
  () => ({ ...values }),
  (now, before) => {
    for (const k of Object.keys(now)) if (now[k] !== before[k]) errors[k] = ""
  },
)

const step = computed(() => props.wizard.steps[index.value]!)
const isLast = computed(() => index.value === props.wizard.steps.length - 1)
const usernameRe = computed(() => new RegExp(props.policy.username_pattern))

// Menambah atau membuang satu nama di isian daftar (dipisah koma).
function toggleListItem(key: string, item: string, on: boolean) {
  const list = splitList(values[key]).filter((x) => x !== item)
  if (on) list.push(item)
  values[key] = list.sort().join(",")
}

// Disk yang dipilih di isian disk-select yang dirujuk f.source.
function sourceDisks(f: WizardField): Disk[] {
  const names = new Set(splitList(values[f.source ?? ""]))
  return (props.context?.disks ?? []).filter((d) => names.has(d.name))
}

function validateField(f: WizardField): string {
  const v = values[f.key] ?? ""
  // Kotak centang kode cadangan punya pesan sendiri (bukan "wajib diisi").
  if (f.type === "recovery-codes") return v === "yes" ? "" : t("recovery.mustConfirm")
  if (f.type === "erase-confirm") return v === "yes" ? "" : t("pool.mustConfirmErase")
  if (f.type === "acknowledge") return v === "yes" ? "" : t("wizard.mustAcknowledge")
  if (f.type === "choice") return f.options?.some((o) => o.value === v) ? "" : t("wizard.required")
  if (f.type === "pool-select") return props.context?.pools?.pools.some((p) => p.name === v) ? "" : t("shares.pickPool")
  if (f.type === "confirm" && f.match_context === "pool") return v === props.context?.pool?.name ? "" : t("wizard.mismatch")
  if (f.type === "disk-select" && f.required && !v) return t("pool.pickDisks")
  if (f.type === "pool-layout") {
    const l = props.context?.pools?.layouts.find((x) => x.id === v)
    return l && fits(l, sourceDisks(f).length) ? "" : t("pool.pickLayout")
  }
  if (f.required && !v.trim()) return t("wizard.required")
  if (f.pattern && !new RegExp(f.pattern).test(v)) return t("wizard.patternMismatch")
  switch (f.type) {
    case "username":
      return usernameRe.value.test(v) ? "" : t("wizard.patternMismatch")
    case "password":
      return [...v].length >= props.policy.password_min_length ? "" : t("wizard.tooShort", { n: props.policy.password_min_length })
    case "password-confirm":
    case "confirm":
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
    // Isian rahasia (password) tidak ikut diteruskan ke pembuka wizard.
    const visible: Record<string, string> = {}
    for (const st of props.wizard.steps) for (const f of st.fields ?? []) if (!f.secret) visible[f.key] = values[f.key] ?? ""
    emit("done", visible)
  } catch (e) {
    const code = e instanceof ApiError ? e.code : "generic"
    // Rahasia 2FA kedaluwarsa di server: daftar ulang saat langkah 2FA dibuka.
    if (code === "totp_expired") {
      // Pendaftaran baru = kode cadangan baru: konfirmasi lama tidak berlaku.
      enrollment.secret = ""
      for (const st of props.wizard.steps) for (const f of st.fields ?? []) if (f.type === "recovery-codes") values[f.key] = ""
    }
    // Pesan dari server (misalnya keluaran zpool) bisa disisipkan terjemahan.
    const params = { message: e instanceof Error ? e.message : "" }
    const msg = t(`errors.${code}`) !== `errors.${code}` ? t(`errors.${code}`, params) : t("errors.generic")
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

// Ringkasan berisi isian dari langkah biasa; isian di langkah ringkasan itu
// sendiri (konfirmasi, password) tidak diulang.
const hiddenInReview = new Set(["password-confirm", "confirm", "erase-confirm", "current-password", "acknowledge"])
const size = (b: number) => formatBytes(b, locale.value, 1000)

const reviewRows = computed(() =>
  props.wizard.steps.flatMap((st) =>
    (st.type === "review" ? [] : (st.fields ?? []))
      .filter((f) => !hiddenInReview.has(f.type))
      .map((f) => {
        let value = values[f.key] ?? ""
        let label = pick(f.label, locale.value)
        if (f.type === "language") value = languages.find((l) => l.code === value)?.label ?? value
        else if (f.type === "disk-select") {
          const names = new Set(splitList(value))
          value = (props.context?.disks ?? [])
            .filter((d) => names.has(d.name))
            .map((d) => `${d.name} (${size(d.size_bytes)})`)
            .join(", ")
        } else if (f.type === "pool-layout") value = value ? t(`pool.layout.${value}.name`) : ""
        else if (f.type === "choice") value = pick(f.options?.find((o) => o.value === value)?.label, locale.value)
        else if (f.type === "group-select") value = splitList(value).join(", ") || "—"
        else if (f.type === "toggle") value = value === "yes" ? t("wizard.yes") : t("wizard.no")
        else if (f.type === "permissions")
          value =
            Object.entries(parsePermissions(value))
              .map(([who, l]) => `${who.split(":")[1]} (${t(`permissions.${l}`)})`)
              .join(", ") || t("permissions.nobodyShort")
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
        <dl v-if="reviewRows.length" class="divide-y rounded-xl border text-sm">
          <div v-for="row in reviewRows" :key="row.key" class="flex justify-between gap-4 px-4 py-3">
            <dt class="text-muted-foreground">{{ row.label }}</dt>
            <dd class="text-right font-medium break-all">{{ row.value }}</dd>
          </div>
        </dl>
      </template>

      <div v-for="f in step.fields ?? []" :key="f.key" class="space-y-1.5">
        <!-- Label kode cadangan ada di samping kotak centangnya. -->
        <Label v-if="!['recovery-codes', 'erase-confirm', 'acknowledge', 'toggle'].includes(f.type)" :for="`wz-${f.key}`">{{ pick(f.label, locale) }}</Label>

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

        <!-- Disk untuk pool -->
        <DiskSelect
          v-else-if="f.type === 'disk-select'"
          v-model="values[f.key]!"
          :disks="context?.disks ?? []"
          :allow-removable="!!context?.pools?.allow_removable"
          :allow-usb="values.allow_usb === 'yes'"
          :max="f.max"
          :input-id="`wz-${f.key}`"
          :invalid="!!errors[f.key]"
        />

        <!-- Susunan pool -->
        <PoolLayoutSelect
          v-else-if="f.type === 'pool-layout'"
          v-model="values[f.key]!"
          :layouts="context?.pools?.layouts ?? []"
          :disks="sourceDisks(f)"
          :input-id="`wz-${f.key}`"
          :invalid="!!errors[f.key]"
        />

        <!-- Konfirmasi penghapusan disk -->
        <EraseConfirm
          v-else-if="f.type === 'erase-confirm'"
          v-model="values[f.key]!"
          :disks="sourceDisks(f)"
          :label="pick(f.label, locale)"
          :input-id="`wz-${f.key}`"
        />

        <!-- Pilihan (misalnya peran pengguna) -->
        <div v-else-if="f.type === 'choice'" :id="`wz-${f.key}`" role="radiogroup" class="space-y-2">
          <button
            v-for="o in f.options ?? []"
            :key="o.value"
            type="button"
            role="radio"
            :aria-checked="values[f.key] === o.value"
            :class="
              cn(
                'flex w-full items-start justify-between gap-3 rounded-lg border px-3 py-2.5 text-left text-sm hover:bg-accent',
                values[f.key] === o.value && 'border-primary ring-1 ring-primary',
              )
            "
            @click="values[f.key] = o.value"
          >
            <span class="space-y-0.5">
              <span class="block font-medium">{{ pick(o.label, locale) }}</span>
              <span v-if="o.help" class="block text-xs text-muted-foreground">{{ pick(o.help, locale) }}</span>
            </span>
            <Check v-if="values[f.key] === o.value" class="size-4 shrink-0 text-primary" />
          </button>
        </div>

        <!-- Grup -->
        <div v-else-if="f.type === 'group-select'" :id="`wz-${f.key}`" class="space-y-2">
          <label v-for="g in context?.groups ?? []" :key="g.name" class="flex items-start gap-2 rounded-lg border px-3 py-2 text-sm">
            <input
              type="checkbox"
              class="mt-0.5 size-4 accent-primary"
              :checked="splitList(values[f.key]).includes(g.name)"
              @change="toggleListItem(f.key, g.name, ($event.target as HTMLInputElement).checked)"
            />
            <span>
              <span class="block font-medium">{{ g.name }}</span>
              <span v-if="g.description" class="block text-xs text-muted-foreground">{{ g.description }}</span>
            </span>
          </label>
          <p v-if="!(context?.groups ?? []).length" class="text-sm text-muted-foreground">{{ t("wizard.noGroups") }}</p>
        </div>

        <!-- Pool -->
        <div v-else-if="f.type === 'pool-select'" :id="`wz-${f.key}`" role="radiogroup" class="space-y-2">
          <button
            v-for="p in context?.pools?.pools ?? []"
            :key="p.name"
            type="button"
            role="radio"
            :aria-checked="values[f.key] === p.name"
            :class="cn('flex w-full items-center justify-between gap-3 rounded-lg border px-3 py-2.5 text-left text-sm hover:bg-accent', values[f.key] === p.name && 'border-primary ring-1 ring-primary')"
            @click="values[f.key] = p.name"
          >
            <span>
              <span class="block font-medium">{{ p.name }}</span>
              <span class="block text-xs text-muted-foreground">{{ t("pool.free", { free: size(p.free_bytes) }) }}</span>
            </span>
            <Check v-if="values[f.key] === p.name" class="size-4 shrink-0 text-primary" />
          </button>
          <p v-if="!(context?.pools?.pools ?? []).length" class="text-sm text-muted-foreground">{{ t("shares.noPool") }}</p>
        </div>

        <!-- Izin folder -->
        <PermissionEditor
          v-else-if="f.type === 'permissions'"
          :model-value="parsePermissions(values[f.key])"
          :users="context?.users ?? []"
          :groups="context?.groups ?? []"
          @update:model-value="(p) => (values[f.key] = formatPermissions(p))"
        />

        <!-- Sakelar ya/tidak -->
        <label v-else-if="f.type === 'toggle'" class="flex items-start gap-2 text-sm">
          <input
            :id="`wz-${f.key}`"
            type="checkbox"
            class="mt-0.5 size-4 accent-primary"
            :checked="values[f.key] === 'yes'"
            @change="values[f.key] = ($event.target as HTMLInputElement).checked ? 'yes' : ''"
          />
          <span>{{ pick(f.label, locale) }}</span>
        </label>

        <!-- Kotak centang pernyataan (misalnya "saya mengerti data hilang") -->
        <label v-else-if="f.type === 'acknowledge'" class="flex items-start gap-2 text-sm">
          <input
            :id="`wz-${f.key}`"
            type="checkbox"
            class="mt-0.5 size-4 accent-destructive"
            :checked="values[f.key] === 'yes'"
            @change="values[f.key] = ($event.target as HTMLInputElement).checked ? 'yes' : ''"
          />
          <span>{{ pick(f.label, locale) }}</span>
        </label>

        <!-- Password -->
        <div v-else-if="f.type === 'password' || f.type === 'password-confirm' || f.type === 'current-password'" class="relative">
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
        <Button v-if="cancelable && index === 0" type="button" variant="ghost" :disabled="busy" @click="emit('cancel')">
          {{ t("common.cancel") }}
        </Button>
        <Button v-else type="button" variant="ghost" :disabled="index === 0 || busy" @click="back">
          <ArrowLeft />{{ t("wizard.back") }}
        </Button>
        <Button type="submit" :disabled="busy" :variant="isLast && wizard.destructive ? 'destructive' : 'default'">
          <template v-if="busy"><LoaderCircle class="animate-spin" />{{ t("wizard.finishing") }}</template>
          <template v-else-if="isLast"><Check />{{ wizard.finish_label ? pick(wizard.finish_label, locale) : t("wizard.finish") }}</template>
          <template v-else>{{ t("wizard.next") }}<ArrowRight /></template>
        </Button>
      </div>
    </form>
  </div>
</template>
