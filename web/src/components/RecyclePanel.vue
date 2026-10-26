<script setup lang="ts">
// Tempat sampah satu shared folder (ADR 0021): nyala/mati, lama simpan
// (password admin), isi saat terakhir diperiksa sapod, dan kosongkan
// sekarang (nama folder diketik ulang + password).
import { reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, LoaderCircle, Trash2, TriangleAlert } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import type { RecycleUsage, SharedFolder } from "@/lib/api"
import { api, ApiError } from "@/lib/api"
import { formatBytes } from "@/lib/format"

const props = defineProps<{ share: SharedFolder; usage?: RecycleUsage }>()
const emit = defineEmits<{ saved: [] }>()
const { t, locale } = useI18n()

const busy = ref("")
const error = ref("")
const notice = ref("")
const emptying = ref(false)
const form = reactive({ enabled: props.share.recycle_days > 0, days: String(props.share.recycle_days || 30), password: "", confirm: "", emptyPassword: "" })

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`, { message: e instanceof Error ? e.message : "" }) : t("errors.generic")
}
const when = (s: string) => new Date(s).toLocaleString(locale.value, { dateStyle: "medium", timeStyle: "short" })
const validDays = () => /^[1-9][0-9]{0,2}$/.test(form.days) && Number(form.days) <= 365
const newDays = () => (form.enabled ? Number(form.days) : 0)

async function run(key: string, fn: () => Promise<string>) {
  busy.value = key
  error.value = notice.value = ""
  try {
    notice.value = await fn()
    emit("saved")
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = ""
  }
}
const save = () =>
  run("save", async () => {
    const res = await api.setShareRecycle(props.share.name, newDays(), form.password)
    form.password = ""
    if (res.warning) throw new ApiError(200, res.warning, "")
    return t("recycle.saved")
  })
const empty = () =>
  run("empty", async () => {
    const res = await api.emptyShareRecycle(props.share.name, form.confirm, form.emptyPassword)
    Object.assign(form, { confirm: "", emptyPassword: "" })
    emptying.value = false
    return res.done ? t("recycle.emptied", { n: res.removed }) : t("recycle.emptyPartial", { n: res.removed })
  })
</script>

<template>
  <section class="space-y-3">
    <h4 class="text-sm font-semibold">{{ t("recycle.title") }}</h4>
    <p class="text-xs text-muted-foreground">{{ t("recycle.help") }}</p>
    <p class="text-sm">{{ share.recycle_days > 0 ? t("recycle.on", { days: share.recycle_days }) : t("recycle.off") }}</p>
    <p v-if="usage && (usage.files || usage.bytes)" class="text-xs">
      {{ t("recycle.usage", { size: formatBytes(usage.bytes, locale, 1000), n: usage.files, when: when(usage.checked) }) }}
    </p>
    <p v-if="share.recycle_error" class="flex items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-xs" role="alert">
      <TriangleAlert class="mt-0.5 size-4 shrink-0 text-destructive" />{{ t("recycle.error", { message: share.recycle_error }) }}
    </p>
    <p v-if="notice" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ notice }}</p>
    <p v-if="error" class="flex items-center gap-2 text-sm text-destructive" role="alert"><TriangleAlert class="size-4" />{{ error }}</p>

    <form class="space-y-3" @submit.prevent="save">
      <label class="flex items-center gap-2 text-sm"><input v-model="form.enabled" type="checkbox" class="size-4 accent-primary" />{{ t("recycle.enable") }}</label>
      <div v-if="form.enabled" class="space-y-1.5">
        <Label :for="`rc-d-${share.name}`">{{ t("recycle.days") }}</Label>
        <Input :id="`rc-d-${share.name}`" v-model="form.days" inputmode="numeric" autocomplete="off" class="w-24" />
      </div>
      <p v-if="form.enabled && share.recycle_days > 0 && validDays() && Number(form.days) < share.recycle_days" class="flex items-center gap-1 text-xs text-amber-700 dark:text-amber-400">
        <TriangleAlert class="size-3.5 shrink-0" />{{ t("recycle.shorter") }}
      </p>
      <p v-if="!form.enabled && share.recycle_days > 0" class="text-xs text-muted-foreground">{{ t("recycle.offKeeps") }}</p>
      <div class="space-y-1.5">
        <Label :for="`rc-p-${share.name}`">{{ t("account.password") }}</Label>
        <Input :id="`rc-p-${share.name}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
      </div>
      <Button type="submit" size="sm" :disabled="!!busy || !form.password || (form.enabled && !validDays()) || newDays() === share.recycle_days">
        <LoaderCircle v-if="busy === 'save'" class="animate-spin" />{{ t("users.save") }}
      </Button>
    </form>

    <div class="border-t pt-3">
      <Button v-if="!emptying" size="sm" variant="outline" class="text-destructive hover:text-destructive" :disabled="!!busy" @click="emptying = true"><Trash2 />{{ t("recycle.empty") }}</Button>
      <form v-else class="space-y-3" @submit.prevent="empty">
        <p class="text-xs text-muted-foreground">{{ t("recycle.emptyHelp", { name: share.name }) }}</p>
        <div class="space-y-1.5">
          <Label :for="`rc-c-${share.name}`">{{ t("recycle.confirmName") }}</Label>
          <Input :id="`rc-c-${share.name}`" v-model="form.confirm" autocomplete="off" class="max-w-xs" />
        </div>
        <div class="space-y-1.5">
          <Label :for="`rc-ep-${share.name}`">{{ t("account.password") }}</Label>
          <Input :id="`rc-ep-${share.name}`" v-model="form.emptyPassword" type="password" autocomplete="current-password" class="max-w-xs" />
        </div>
        <div class="flex gap-2">
          <Button type="submit" size="sm" variant="destructive" :disabled="!!busy || form.confirm !== share.name || !form.emptyPassword">
            <LoaderCircle v-if="busy === 'empty'" class="animate-spin" />{{ t("recycle.empty") }}
          </Button>
          <Button type="button" size="sm" variant="ghost" @click="emptying = false">{{ t("common.cancel") }}</Button>
        </div>
      </form>
    </div>
  </section>
</template>
