<script setup lang="ts">
// Penyimpanan eksternal (ADR 0015): drive USB yang tercolok, dikelompokkan
// per perangkat. Drive exFAT/FAT/NTFS dipasang sebagai folder eksternal
// (izin untuk satu grup, baca saja atau baca-tulis, opsional SMB) dan
// dilepas dengan aman. Daftar diperbarui berkala (colok/cabut).
import { computed, onMounted, onUnmounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, Eject, LoaderCircle, Plug, TriangleAlert, Usb } from "@lucide/vue"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import type { ExternalVolume, UserGroup } from "@/lib/api"
import { api, ApiError } from "@/lib/api"
import { formatBytes } from "@/lib/format"

const { t, locale } = useI18n()
const vols = ref<ExternalVolume[] | null>(null)
const groups = ref<UserGroup[]>([])
const error = ref("")
const notice = ref("")
const busy = ref("")

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`, { message: e instanceof Error ? e.message : "" }) : t("errors.generic")
}
async function load() {
  try {
    vols.value = await api.externalVolumes()
  } catch (e) {
    error.value = message(e)
  }
}
let timer: number | undefined
onMounted(async () => {
  load()
  try {
    groups.value = await api.groups()
  } catch {
    // daftar grup opsional
  }
  timer = window.setInterval(load, 10000)
})
onUnmounted(() => window.clearInterval(timer))

const size = (b: number) => formatBytes(b, locale.value, 1000)
// Per perangkat USB (disk), agar dock 2-bay tampil sebagai satu kelompok.
const byDisk = computed(() => {
  const m = new Map<string, ExternalVolume[]>()
  for (const v of vols.value ?? []) {
    const k = v.disk || v.name || v.uuid
    m.set(k, [...(m.get(k) ?? []), v])
  }
  return [...m.entries()]
})
const fsLabel = (fs: string) => ({ exfat: "exFAT", vfat: "FAT32", ntfs: "NTFS" })[fs] ?? fs

const open = ref("")
const form = reactive({ name: "", group: "sapo-users", readOnly: false, smb: true, password: "" })
function start(v: ExternalVolume) {
  open.value = v.uuid
  error.value = notice.value = ""
  const base = (v.label || v.model || "usb").toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^[^a-z]+/, "").slice(0, 24)
  Object.assign(form, { name: base ? `usb-${base}`.slice(0, 32) : "usb", group: "sapo-users", readOnly: false, smb: true, password: "" })
}
async function run(key: string, fn: () => Promise<unknown>, done: string) {
  busy.value = key
  error.value = notice.value = ""
  try {
    await fn()
    notice.value = done
    open.value = ""
    await load()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = ""
  }
}
const mount = (v: ExternalVolume) =>
  run(
    v.uuid,
    () => api.mountExternal({ uuid: v.uuid, name: form.name.trim(), read_only: form.readOnly, group: form.group, smb: form.smb, admin_password: form.password }),
    t("external.mounted", { name: form.name.trim() }),
  )
const unmount = (v: ExternalVolume) => run(v.uuid, () => api.unmountExternal(v.name!), t("external.unmounted", { name: v.name }))
</script>

<template>
  <section class="space-y-3">
    <h3 class="flex items-center gap-2 pt-2 text-sm font-semibold"><Usb class="size-4" />{{ t("external.title") }}</h3>
    <p class="text-xs text-muted-foreground">{{ t("external.help") }}</p>
    <p v-if="notice" class="flex items-center gap-2 rounded-lg border border-emerald-500/40 bg-emerald-500/5 p-2.5 text-sm"><CircleCheck class="size-4 text-emerald-600" />{{ notice }}</p>
    <p v-if="error" class="flex items-center gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-sm" role="alert"><TriangleAlert class="size-4" />{{ error }}</p>
    <p v-if="vols && !vols.length" class="rounded-lg bg-muted p-3 text-sm">{{ t("external.none") }}</p>

    <ul class="space-y-3">
      <li v-for="[disk, list] in byDisk" :key="disk" class="space-y-2 rounded-xl border p-3">
        <p class="text-xs text-muted-foreground">
          <Usb class="mr-1 inline size-3.5" />{{ list[0]!.model || disk }}<template v-if="list[0]!.disk"> · <code class="font-mono">{{ list[0]!.disk }}</code></template>
        </p>
        <div v-for="v in list" :key="v.uuid || v.device" class="space-y-2 rounded-lg border p-2.5">
          <div class="flex flex-wrap items-start justify-between gap-2">
            <div class="min-w-0 space-y-0.5">
              <p class="text-sm font-medium">
                {{ v.name || v.label || v.device }}
                <Badge v-if="v.state === 'mounted'" variant="secondary" class="ml-1">{{ t("external.state.mounted") }}</Badge>
                <Badge v-else-if="v.state === 'disconnected'" variant="outline" class="ml-1">{{ t("external.state.disconnected") }}</Badge>
              </p>
              <p class="text-xs text-muted-foreground">
                {{ fsLabel(v.fstype) }}<template v-if="v.size_bytes"> · {{ size(v.size_bytes) }}</template><template v-if="v.label && v.name"> · {{ v.label }}</template>
                <template v-if="v.state === 'mounted' && v.available_bytes !== undefined"> · {{ t("external.free", { free: size(v.available_bytes) }) }}</template>
              </p>
              <p v-if="v.name" class="text-xs">
                {{ v.group === "sapo-users" ? t("external.allUsers") : v.group }} · {{ v.read_only ? t("permissions.read") : t("permissions.write") }}<template v-if="v.smb"> · SMB</template>
              </p>
              <p v-if="!v.supported && v.reason" class="text-xs text-muted-foreground">{{ t(`external.reason.${v.reason}`) }}</p>
              <p v-if="v.error" class="text-xs text-destructive">{{ v.error }}</p>
            </div>
            <div v-if="open !== v.uuid" class="flex gap-1">
              <Button v-if="!v.name && v.supported" size="sm" variant="outline" @click="start(v)"><Plug />{{ t("external.mount") }}</Button>
              <Button v-if="v.name" size="sm" variant="outline" :disabled="busy === v.uuid" @click="unmount(v)">
                <LoaderCircle v-if="busy === v.uuid" class="animate-spin" /><Eject v-else />{{ v.state === "mounted" ? t("external.eject") : t("external.forget") }}
              </Button>
            </div>
          </div>
          <form v-if="open === v.uuid" class="space-y-3 border-t pt-3" @submit.prevent="mount(v)">
            <div class="grid gap-3 sm:grid-cols-2">
              <div class="space-y-1.5">
                <Label :for="`ex-n-${v.uuid}`">{{ t("external.name") }}</Label>
                <Input :id="`ex-n-${v.uuid}`" v-model="form.name" pattern="^[a-z][a-z0-9_-]{0,31}$" autocomplete="off" />
              </div>
              <div class="space-y-1.5">
                <Label :for="`ex-g-${v.uuid}`">{{ t("external.group") }}</Label>
                <select
                  :id="`ex-g-${v.uuid}`"
                  v-model="form.group"
                  class="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm shadow-xs focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
                >
                  <option value="sapo-users">{{ t("external.allUsers") }}</option>
                  <option v-for="g in groups" :key="g.name" :value="g.name">{{ g.name }}</option>
                </select>
              </div>
            </div>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.readOnly" type="checkbox" class="size-4 accent-primary" />{{ t("external.readOnly") }}</label>
            <label class="flex items-center gap-2 text-sm"><input v-model="form.smb" type="checkbox" class="size-4 accent-primary" />{{ t("external.smb") }}</label>
            <div class="space-y-1.5">
              <Label :for="`ex-p-${v.uuid}`">{{ t("account.password") }}</Label>
              <Input :id="`ex-p-${v.uuid}`" v-model="form.password" type="password" autocomplete="current-password" class="max-w-xs" />
            </div>
            <div class="flex gap-2">
              <Button type="submit" size="sm" :disabled="!!busy || !form.password || !form.name.trim()"><LoaderCircle v-if="busy === v.uuid" class="animate-spin" />{{ t("external.mount") }}</Button>
              <Button type="button" size="sm" variant="ghost" @click="open = ''">{{ t("common.cancel") }}</Button>
            </div>
          </form>
        </div>
      </li>
    </ul>
  </section>
</template>
