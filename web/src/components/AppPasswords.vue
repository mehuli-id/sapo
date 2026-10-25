<script setup lang="ts">
// Password aplikasi untuk WebDAV (Sapo Drive): satu per perangkat, hanya
// berlaku untuk WebDAV, ditampilkan sekali saat dibuat, bisa dicabut.
import { onMounted, ref } from "vue"
import { useI18n } from "vue-i18n"
import { Copy, KeyRound, LoaderCircle, Trash2 } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import type { AppPassword } from "@/lib/api"
import { api, ApiError } from "@/lib/api"

const { t, locale } = useI18n()
const list = ref<AppPassword[]>([])
const adding = ref(false)
const name = ref("")
const password = ref("")
const busy = ref(false)
const error = ref("")
const created = ref<{ name: string; password: string } | null>(null)
const copied = ref(false)

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`) : t("errors.generic")
}
async function load() {
  try {
    list.value = await api.appPasswords()
  } catch (e) {
    error.value = message(e)
  }
}
onMounted(load)

const date = (s?: string) => (s ? new Date(s).toLocaleString(locale.value, { dateStyle: "medium", timeStyle: "short" }) : t("appPasswords.never"))

async function create() {
  busy.value = true
  error.value = ""
  try {
    const res = await api.createAppPassword(name.value.trim(), password.value)
    created.value = { name: res.name, password: res.password }
    copied.value = false
    adding.value = false
    name.value = password.value = ""
    await load()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}
async function revoke(id: string) {
  error.value = ""
  try {
    await api.revokeAppPassword(id)
    await load()
  } catch (e) {
    error.value = message(e)
  }
}
async function copy() {
  try {
    await navigator.clipboard.writeText(created.value!.password)
    copied.value = true
  } catch {
    // clipboard tidak tersedia
  }
}
</script>

<template>
  <section class="space-y-3 rounded-xl border p-4">
    <h3 class="flex items-center gap-2 font-medium"><KeyRound class="size-4" />{{ t("appPasswords.title") }}</h3>
    <p class="text-xs text-muted-foreground">{{ t("appPasswords.help") }}</p>

    <div v-if="created" class="space-y-2 rounded-lg border border-primary/40 bg-primary/5 p-3">
      <p class="text-sm">{{ t("appPasswords.created", { name: created.name }) }}</p>
      <div class="flex items-center gap-2">
        <code class="rounded bg-muted px-2 py-1 font-mono text-sm tracking-wider">{{ created.password }}</code>
        <button type="button" class="rounded p-1 hover:bg-accent" :aria-label="t('recovery.copy')" @click="copy"><Copy class="size-3.5" /></button>
        <span v-if="copied" class="text-xs text-muted-foreground">{{ t("recovery.copied") }}</span>
      </div>
      <p class="text-xs text-muted-foreground">{{ t("appPasswords.onlyOnce") }}</p>
      <Button size="sm" variant="ghost" @click="created = null">{{ t("account.done") }}</Button>
    </div>

    <ul v-if="list.length" class="divide-y">
      <li v-for="ap in list" :key="ap.id" class="flex items-center justify-between gap-2 py-2">
        <div class="min-w-0">
          <p class="truncate text-sm">{{ ap.name }}</p>
          <p class="text-xs text-muted-foreground">{{ t("appPasswords.lastUsed", { when: date(ap.last_used_at) }) }}</p>
        </div>
        <Button size="sm" variant="ghost" class="text-destructive hover:text-destructive" :aria-label="t('appPasswords.revoke')" @click="revoke(ap.id)"><Trash2 /></Button>
      </li>
    </ul>
    <p v-else-if="!adding" class="text-xs text-muted-foreground">{{ t("appPasswords.none") }}</p>

    <form v-if="adding" class="space-y-2" @submit.prevent="create">
      <div class="space-y-1.5">
        <Label for="ap-name">{{ t("appPasswords.device") }}</Label>
        <Input id="ap-name" v-model="name" maxlength="64" autocomplete="off" :placeholder="t('appPasswords.devicePlaceholder')" class="max-w-xs" />
      </div>
      <div class="space-y-1.5">
        <Label for="ap-pw">{{ t("account.password") }}</Label>
        <Input id="ap-pw" v-model="password" type="password" autocomplete="current-password" class="max-w-xs" />
      </div>
      <div class="flex gap-2">
        <Button type="submit" size="sm" :disabled="busy || !name.trim() || !password"><LoaderCircle v-if="busy" class="animate-spin" />{{ t("appPasswords.create") }}</Button>
        <Button type="button" size="sm" variant="ghost" @click="adding = false">{{ t("account.cancel") }}</Button>
      </div>
    </form>
    <Button v-else size="sm" variant="outline" @click="(adding = true), (created = null)">{{ t("appPasswords.add") }}</Button>
    <p v-if="error" class="text-sm text-destructive" role="alert">{{ error }}</p>
  </section>
</template>
