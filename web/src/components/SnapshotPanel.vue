<script setup lang="ts">
// Snapshot satu folder (ADR 0014): jadwal (templat dari sapo.yaml, angka
// sendiri, atau mati), daftar snapshot, ambil sekarang, dan hapus (password
// admin). Bila policyOnly, hanya jadwal (folder pribadi: satu jadwal untuk
// semua pengguna).
import { computed, onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { Camera, CircleCheck, LoaderCircle, Trash2, TriangleAlert } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import type { SnapshotInfo, SnapshotPolicy, SnapshotPreset, SnapshotTarget } from "@/lib/api"
import { api, ApiError } from "@/lib/api"
import { formatBytes } from "@/lib/format"

const props = defineProps<{
  target?: SnapshotTarget
  policy?: SnapshotPolicy | null
  policyOnly?: boolean
  // listOnly: hanya daftar (jadwal diatur di tempat lain).
  listOnly?: boolean
  save?: (p: SnapshotPolicy | null) => Promise<{ warning?: string }>
}>()
const emit = defineEmits<{ saved: [] }>()

const { t, locale } = useI18n()
const presets = ref<SnapshotPreset[]>([])
const list = ref<SnapshotInfo[] | null>(null)
const usedBySnapshots = ref(0)
const busy = ref("")
const error = ref("")
const notice = ref("")

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`, { message: e instanceof Error ? e.message : "" }) : t("errors.generic")
}
const loc = (m: Record<string, string>) => m[locale.value] ?? m.en ?? Object.values(m)[0] ?? ""

// Pilihan: "off", id templat, atau "custom".
const same = (a: SnapshotPolicy, b: SnapshotPolicy) => a.hourly === b.hourly && a.daily === b.daily && a.weekly === b.weekly && a.hour === b.hour
const form = reactive({ choice: "off", hourly: 0, daily: 0, weekly: 0, hour: 2 })
function reset() {
  const p = props.policy
  Object.assign(form, p ? { ...p, choice: presets.value.find((x) => same(x, p))?.id ?? "custom" } : { choice: "off", hourly: 0, daily: 0, weekly: 0, hour: 2 })
}
const chosen = computed<SnapshotPolicy | null>(() => {
  if (form.choice === "off") return null
  const pr = presets.value.find((x) => x.id === form.choice)
  if (pr) return { hourly: pr.hourly, daily: pr.daily, weekly: pr.weekly, hour: pr.hour }
  return { hourly: Number(form.hourly) || 0, daily: Number(form.daily) || 0, weekly: Number(form.weekly) || 0, hour: Number(form.hour) || 0 }
})
const summary = (p?: SnapshotPolicy | null) =>
  p ? t("snapshots.summary", { hourly: p.hourly, daily: p.daily, weekly: p.weekly, hour: String(p.hour).padStart(2, "0") }) : t("snapshots.off")

async function load() {
  try {
    presets.value = await api.snapshotPresets()
    reset()
    if (props.target && !props.policyOnly) {
      const res = await api.snapshots(props.target)
      list.value = res.snapshots
      usedBySnapshots.value = res.used_by_snapshots
    }
  } catch (e) {
    error.value = message(e)
  }
}
onMounted(load)

async function run(name: string, fn: () => Promise<unknown>, done: string) {
  busy.value = name
  error.value = notice.value = ""
  try {
    const res = (await fn()) as { warning?: string } | undefined
    if (res?.warning) throw new ApiError(200, res.warning, "")
    notice.value = done
    await load()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = ""
  }
}
const savePolicy = () =>
  run("policy", async () => {
    const res = await props.save!(chosen.value)
    emit("saved")
    return res
  }, t("snapshots.saved"))
const takeNow = () => run("create", () => api.createSnapshot(props.target!), t("snapshots.taken"))
const del = reactive({ name: "", password: "" })
const remove = () =>
  run("delete", async () => {
    await api.deleteSnapshot(props.target!, del.name, del.password)
    del.name = del.password = ""
  }, t("snapshots.deleted"))

const when = (s: string) => new Date(s).toLocaleString(locale.value, { dateStyle: "medium", timeStyle: "short" })
</script>

<template>
  <div class="space-y-3">
    <p v-if="!listOnly" class="text-xs text-muted-foreground">{{ t("snapshots.help") }}</p>
    <form v-if="!listOnly" class="space-y-2" @submit.prevent="savePolicy">
      <Label for="snap-choice">{{ t("snapshots.schedule") }}</Label>
      <select
        id="snap-choice"
        v-model="form.choice"
        class="h-9 w-full max-w-md rounded-md border border-input bg-transparent px-3 text-sm shadow-xs focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
      >
        <option value="off">{{ t("snapshots.off") }}</option>
        <option v-for="p in presets" :key="p.id" :value="p.id">{{ loc(p.label) }}</option>
        <option value="custom">{{ t("snapshots.custom") }}</option>
      </select>
      <div v-if="form.choice === 'custom'" class="grid max-w-md grid-cols-2 gap-2 sm:grid-cols-4">
        <div v-for="k in ['hourly', 'daily', 'weekly', 'hour'] as const" :key="k" class="space-y-1">
          <Label :for="`snap-${k}`" class="text-xs">{{ t(`snapshots.field.${k}`) }}</Label>
          <Input :id="`snap-${k}`" v-model.number="form[k]" type="number" min="0" :max="k === 'hour' ? 23 : 520" />
        </div>
      </div>
      <p class="text-xs text-muted-foreground">{{ summary(chosen) }}</p>
      <p v-if="!chosen && policy" class="text-xs">{{ t("snapshots.offKeeps") }}</p>
      <Button type="submit" size="sm" :disabled="!!busy"><LoaderCircle v-if="busy === 'policy'" class="animate-spin" />{{ t("snapshots.saveSchedule") }}</Button>
    </form>

    <template v-if="target && !policyOnly">
      <div :class="['flex flex-wrap items-center justify-between gap-2', !listOnly && 'border-t pt-3']">
        <p class="text-sm font-medium">{{ t("snapshots.list", { n: list?.length ?? 0, size: formatBytes(usedBySnapshots, locale, 1000) }) }}</p>
        <Button size="sm" variant="outline" :disabled="!!busy" @click="takeNow"><LoaderCircle v-if="busy === 'create'" class="animate-spin" /><Camera v-else />{{ t("snapshots.takeNow") }}</Button>
      </div>
      <ul v-if="list?.length" class="max-h-64 divide-y overflow-y-auto">
        <li v-for="s in list" :key="s.name" class="space-y-2 py-1.5">
          <div class="flex items-center justify-between gap-2">
            <p class="text-sm">{{ when(s.created) }} <span class="text-xs text-muted-foreground">· {{ t(`snapshots.kind.${s.kind}`) }} · {{ formatBytes(s.used_bytes, locale, 1000) }}</span></p>
            <Button v-if="del.name !== s.name" size="sm" variant="ghost" class="text-destructive hover:text-destructive" :aria-label="t('snapshots.delete')" @click="(del.name = s.name), (del.password = '')"><Trash2 /></Button>
          </div>
          <form v-if="del.name === s.name" class="flex flex-wrap items-end gap-2" @submit.prevent="remove">
            <div class="space-y-1">
              <Label :for="`snap-pw-${s.name}`" class="text-xs">{{ t("snapshots.deleteHelp") }}</Label>
              <Input :id="`snap-pw-${s.name}`" v-model="del.password" type="password" autocomplete="current-password" :placeholder="t('account.password')" class="max-w-xs" />
            </div>
            <Button type="submit" size="sm" variant="destructive" :disabled="!!busy || !del.password"><LoaderCircle v-if="busy === 'delete'" class="animate-spin" />{{ t("snapshots.delete") }}</Button>
            <Button type="button" size="sm" variant="ghost" @click="del.name = ''">{{ t("common.cancel") }}</Button>
          </form>
        </li>
      </ul>
      <p v-else-if="list" class="text-xs text-muted-foreground">{{ t("snapshots.none") }}</p>
    </template>
    <p v-if="notice" class="flex items-center gap-2 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ notice }}</p>
    <p v-if="error" class="flex items-center gap-2 text-sm text-destructive" role="alert"><TriangleAlert class="size-4" />{{ error }}</p>
  </div>
</template>
