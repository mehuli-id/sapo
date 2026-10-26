<script setup lang="ts">
// Time Machine untuk Mac (ADR 0022): folder menjadi tujuan backup Mac
// dengan kuota (password admin), dan petunjuk memilihnya di Mac.
import { reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, LoaderCircle, TriangleAlert } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import type { SharedFolder } from "@/lib/api"
import { api, ApiError } from "@/lib/api"

const props = defineProps<{ share: SharedFolder }>()
const emit = defineEmits<{ saved: [] }>()
const { t } = useI18n()

const busy = ref(false)
const error = ref("")
const notice = ref("")
const form = reactive({ enabled: !!props.share.time_machine, quota: props.share.time_machine ? String(props.share.time_machine.quota_gb) : "", password: "" })

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`, { message: e instanceof Error ? e.message : "" }) : t("errors.generic")
}
const validQuota = () => /^[1-9][0-9]{1,6}$/.test(form.quota)
const unchanged = () => form.enabled === !!props.share.time_machine && (!form.enabled || Number(form.quota) === props.share.time_machine?.quota_gb)
const writers = () => Object.values(props.share.permissions).includes("write")

async function save() {
  busy.value = true
  error.value = notice.value = ""
  try {
    const res = await api.setShareTimeMachine(props.share.name, form.enabled, form.enabled ? Number(form.quota) : 0, form.password)
    form.password = ""
    if (res.warning) throw new ApiError(200, res.warning, "")
    notice.value = t("timeMachine.saved")
    emit("saved")
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <section class="space-y-3">
    <h4 class="text-sm font-semibold">{{ t("timeMachine.title") }}</h4>
    <p class="text-xs text-muted-foreground">{{ t("timeMachine.help") }}</p>
    <p class="text-sm">{{ share.time_machine ? t("timeMachine.on", { quota: share.time_machine.quota_gb }) : t("timeMachine.off") }}</p>
    <template v-if="share.time_machine">
      <p class="rounded-lg bg-muted p-2.5 text-xs">{{ t("timeMachine.howTo", { name: share.name }) }}</p>
      <p v-if="!writers()" class="flex items-center gap-2 rounded-lg border border-amber-500/40 bg-amber-500/5 p-2.5 text-xs">
        <TriangleAlert class="size-4 shrink-0 text-amber-600" />{{ t("timeMachine.noWriters") }}
      </p>
    </template>
    <p v-if="share.time_machine_error" class="flex items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-xs" role="alert">
      <TriangleAlert class="mt-0.5 size-4 shrink-0 text-destructive" />{{ t("timeMachine.error", { message: share.time_machine_error }) }}
    </p>
    <p v-if="notice" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ notice }}</p>
    <p v-if="error" class="flex items-center gap-2 text-sm text-destructive" role="alert"><TriangleAlert class="size-4" />{{ error }}</p>

    <form class="space-y-3" @submit.prevent="save">
      <label class="flex items-center gap-2 text-sm"><input v-model="form.enabled" type="checkbox" class="size-4 accent-primary" />{{ t("timeMachine.enable") }}</label>
      <div v-if="form.enabled" class="space-y-1.5">
        <Label :for="`tm-q-${share.name}`">{{ t("timeMachine.quota") }}</Label>
        <Input :id="`tm-q-${share.name}`" v-model="form.quota" inputmode="numeric" autocomplete="off" class="w-32" />
        <p class="text-xs text-muted-foreground">{{ t("timeMachine.quotaHelp") }}</p>
      </div>
      <p v-if="!form.enabled && share.time_machine" class="text-xs text-muted-foreground">{{ t("timeMachine.offHelp") }}</p>
      <div class="space-y-1.5">
        <Label :for="`tm-p-${share.name}`">{{ t("account.password") }}</Label>
        <Input :id="`tm-p-${share.name}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
      </div>
      <Button type="submit" size="sm" :disabled="busy || !form.password || (form.enabled && !validQuota()) || unchanged()">
        <LoaderCircle v-if="busy" class="animate-spin" />{{ t("users.save") }}
      </Button>
    </form>
  </section>
</template>
