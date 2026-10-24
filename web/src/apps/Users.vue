<script setup lang="ts">
// Pengguna & Grup (hanya admin). Menambah pengguna lewat wizard
// (wizards/user-create.yaml); tindakan lain langsung di daftar. Tindakan yang
// memberi hak atau mencabut akses meminta password admin lagi.
import { computed, onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, LoaderCircle, Plus, RefreshCw, ShieldCheck, Trash2, UserRound, UsersRound } from "@lucide/vue"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { useSession } from "@/composables/useSession"
import type { UserAccount, UserGroup, Wizard } from "@/lib/api"
import { api, ApiError, wizardActions } from "@/lib/api"
import { cn } from "@/lib/utils"
import WizardRunner from "@/wizard/WizardRunner.vue"

const { t, locale } = useI18n()
const { status, policy } = useSession()
const me = computed(() => status.value?.user?.username)

const tab = ref<"users" | "groups">("users")
const users = ref<UserAccount[] | null>(null)
const groups = ref<UserGroup[] | null>(null)
const loadError = ref("")
const notice = ref("")
const wizard = ref<Wizard | null>(null)

async function load() {
  try {
    ;[users.value, groups.value] = await Promise.all([api.users(), api.groups()])
    loadError.value = ""
  } catch (e) {
    loadError.value = message(e)
  }
}
onMounted(load)

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`) : t("errors.generic")
}

const date = (s?: string) => (s ? new Intl.DateTimeFormat(locale.value, { dateStyle: "medium", timeStyle: "short" }).format(new Date(s)) : t("users.never"))

// --- Tambah pengguna (wizard) ---
async function openWizard() {
  notice.value = ""
  try {
    await load()
    wizard.value = await api.wizard("user-create")
  } catch (e) {
    loadError.value = message(e)
  }
}
async function submitWizard(values: Record<string, string>) {
  return wizardActions[wizard.value!.action]!(values, { groups: groups.value ?? [] })
}
function wizardDone(values: Record<string, string>) {
  wizard.value = null
  notice.value = t("users.created", { name: values.username })
  load()
}

// --- Tindakan per pengguna ---
type Mode = "" | "edit" | "role" | "enable" | "password" | "delete"
const open = ref("")
const mode = ref<Mode>("")
const busy = ref(false)
const error = ref("")
const form = reactive({ description: "", groups: [] as string[], adminPassword: "", password: "", confirm: "", mustChange: true, confirmName: "" })

function start(u: UserAccount, m: Mode) {
  open.value = u.username
  mode.value = m
  error.value = ""
  notice.value = ""
  Object.assign(form, { description: u.description, groups: [...u.groups], adminPassword: "", password: "", confirm: "", mustChange: true, confirmName: "" })
}
function close() {
  open.value = ""
  mode.value = ""
}

async function run(fn: () => Promise<unknown>, done: string) {
  busy.value = true
  error.value = ""
  try {
    await fn()
    notice.value = done
    close()
    await load()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

const saveEdit = (u: UserAccount) =>
  run(() => api.updateUser(u.username, { description: form.description, groups: form.groups }), t("users.saved", { name: u.username }))
const disable = (u: UserAccount) => run(() => api.updateUser(u.username, { disabled: true }), t("users.disabledDone", { name: u.username }))
// Mengaktifkan lagi memberi akses, jadi butuh password admin.
const enable = (u: UserAccount) =>
  run(() => api.updateUser(u.username, { disabled: false, admin_password: form.adminPassword }), t("users.enabledDone", { name: u.username }))
const setRole = (u: UserAccount) =>
  run(
    () => api.updateUser(u.username, { role: u.role === "admin" ? "user" : "admin", admin_password: form.adminPassword }),
    t("users.roleDone", { name: u.username }),
  )
const resetPassword = (u: UserAccount) =>
  run(
    () =>
      api.resetUserPassword(u.username, {
        password: form.password,
        password_confirm: form.confirm,
        must_change_password: form.mustChange,
        admin_password: form.adminPassword,
      }),
    t("users.passwordDone", { name: u.username }),
  )
const remove = (u: UserAccount) =>
  run(() => api.deleteUser(u.username, { confirm_name: form.confirmName, admin_password: form.adminPassword }), t("users.deleted", { name: u.username }))

function toggleIn(list: string[], item: string, on: boolean) {
  const i = list.indexOf(item)
  if (on && i < 0) list.push(item)
  if (!on && i >= 0) list.splice(i, 1)
}

// --- Grup ---
const newGroup = reactive({ name: "", description: "" })
const members = reactive<Record<string, string[]>>({})
const deleting = ref("")

const createGroup = () =>
  run(async () => {
    await api.createGroup(newGroup.name.trim().toLowerCase(), newGroup.description)
    Object.assign(newGroup, { name: "", description: "" })
  }, t("groups.created"))
function editMembers(g: UserGroup) {
  members[g.name] = [...g.members]
}
const saveMembers = (g: UserGroup) =>
  run(async () => {
    await api.updateGroup(g.name, { members: members[g.name] })
    delete members[g.name]
  }, t("groups.saved", { name: g.name }))
const deleteGroup = (g: UserGroup) =>
  run(async () => {
    await api.deleteGroup(g.name)
    deleting.value = ""
  }, t("groups.deleted", { name: g.name }))
</script>

<template>
  <div v-if="wizard" class="p-4">
    <h2 class="mb-4 text-sm font-medium text-muted-foreground">{{ wizard.title[locale] ?? wizard.title.en }}</h2>
    <WizardRunner :wizard="wizard" :policy="policy" :submit="submitWizard" :context="{ groups: groups ?? [] }" cancelable @cancel="wizard = null" @done="wizardDone" />
  </div>

  <div v-else class="space-y-4 p-4">
    <div class="flex items-center justify-between gap-3">
      <div class="flex gap-1 rounded-lg bg-muted p-1 text-sm" role="tablist">
        <button
          v-for="id in ['users', 'groups'] as const"
          :key="id"
          role="tab"
          :aria-selected="tab === id"
          :class="cn('rounded-md px-3 py-1', tab === id ? 'bg-background font-medium shadow-sm' : 'text-muted-foreground')"
          @click="tab = id"
        >
          {{ t(`users.tab.${id}`) }}
        </button>
      </div>
      <div class="flex gap-2">
        <Button size="sm" variant="ghost" @click="load"><RefreshCw />{{ t("storage.refresh") }}</Button>
        <Button v-if="tab === 'users'" size="sm" @click="openWizard"><Plus />{{ t("users.add") }}</Button>
      </div>
    </div>

    <p v-if="notice" class="flex items-center gap-2 rounded-lg border border-emerald-500/40 bg-emerald-500/5 p-3 text-sm">
      <CircleCheck class="size-4 shrink-0 text-emerald-600" />{{ notice }}
    </p>
    <p v-if="loadError" class="rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm">{{ loadError }}</p>

    <!-- Pengguna -->
    <ul v-if="tab === 'users' && users" class="space-y-2">
      <li v-for="u in users" :key="u.username" class="space-y-3 rounded-xl border p-3">
        <div class="flex flex-wrap items-start justify-between gap-2">
          <div class="flex min-w-0 items-start gap-3">
            <UserRound class="mt-0.5 size-5 shrink-0 text-muted-foreground" />
            <div class="min-w-0 space-y-0.5">
              <div class="flex flex-wrap items-center gap-1.5">
                <span class="font-medium">{{ u.username }}</span>
                <Badge v-if="u.role === 'admin'" variant="secondary"><ShieldCheck />{{ t("users.role.admin") }}</Badge>
                <Badge v-if="u.disabled" variant="destructive">{{ t("users.disabled") }}</Badge>
                <Badge v-if="u.pending.length" variant="outline" class="border-amber-500/60 text-amber-700 dark:text-amber-400">{{ t("users.notActivated") }}</Badge>
                <Badge v-if="u.two_factor" variant="outline">2FA</Badge>
                <Badge v-if="u.username === me" variant="outline">{{ t("users.you") }}</Badge>
              </div>
              <p v-if="u.description" class="text-xs text-muted-foreground">{{ u.description }}</p>
              <p class="text-xs text-muted-foreground">
                {{ t("users.groups") }}: {{ u.groups.length ? u.groups.join(", ") : "—" }} · {{ t("users.lastLogin") }}: {{ date(u.last_login_at) }}
              </p>
            </div>
          </div>
          <div v-if="open !== u.username" class="flex flex-wrap gap-1">
            <Button size="sm" variant="outline" @click="start(u, 'edit')">{{ t("users.edit") }}</Button>
            <template v-if="u.username !== me">
              <Button size="sm" variant="outline" @click="start(u, 'password')">{{ t("users.resetPassword") }}</Button>
              <Button size="sm" variant="outline" @click="start(u, 'role')">{{ u.role === "admin" ? t("users.makeUser") : t("users.makeAdmin") }}</Button>
              <Button v-if="u.disabled" size="sm" variant="outline" @click="start(u, 'enable')">{{ t("users.enable") }}</Button>
              <Button v-else size="sm" variant="outline" :disabled="busy" @click="disable(u)">{{ t("users.disable") }}</Button>
              <Button size="sm" variant="ghost" class="text-destructive hover:text-destructive" @click="start(u, 'delete')"><Trash2 /></Button>
            </template>
          </div>
        </div>

        <!-- Ubah keterangan dan grup -->
        <form v-if="open === u.username && mode === 'edit'" class="space-y-3 border-t pt-3" @submit.prevent="saveEdit(u)">
          <div class="space-y-1.5">
            <Label :for="`desc-${u.username}`">{{ t("users.description") }}</Label>
            <Input :id="`desc-${u.username}`" v-model="form.description" maxlength="128" />
          </div>
          <div class="space-y-1.5">
            <Label>{{ t("users.groups") }}</Label>
            <p v-if="!groups?.length" class="text-xs text-muted-foreground">{{ t("wizard.noGroups") }}</p>
            <div class="flex flex-wrap gap-3">
              <label v-for="g in groups ?? []" :key="g.name" class="flex items-center gap-1.5 text-sm">
                <input
                  type="checkbox"
                  class="size-4 accent-primary"
                  :checked="form.groups.includes(g.name)"
                  @change="toggleIn(form.groups, g.name, ($event.target as HTMLInputElement).checked)"
                />{{ g.name }}
              </label>
            </div>
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="busy"><LoaderCircle v-if="busy" class="animate-spin" />{{ t("users.save") }}</Button>
            <Button type="button" size="sm" variant="ghost" @click="close">{{ t("common.cancel") }}</Button>
          </div>
        </form>

        <!-- Ubah peran -->
        <form v-else-if="open === u.username && mode === 'role'" class="space-y-3 border-t pt-3" @submit.prevent="setRole(u)">
          <p class="text-sm">{{ u.role === "admin" ? t("users.makeUserHelp") : t("users.makeAdminHelp") }}</p>
          <div class="space-y-1.5">
            <Label :for="`ap-${u.username}`">{{ t("account.password") }}</Label>
            <Input :id="`ap-${u.username}`" v-model="form.adminPassword" type="password" autocomplete="current-password" />
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="busy || !form.adminPassword">
              <LoaderCircle v-if="busy" class="animate-spin" />{{ u.role === "admin" ? t("users.makeUser") : t("users.makeAdmin") }}
            </Button>
            <Button type="button" size="sm" variant="ghost" @click="close">{{ t("common.cancel") }}</Button>
          </div>
        </form>

        <!-- Aktifkan lagi -->
        <form v-else-if="open === u.username && mode === 'enable'" class="space-y-3 border-t pt-3" @submit.prevent="enable(u)">
          <p class="text-sm">{{ t("users.enableHelp") }}</p>
          <div class="space-y-1.5">
            <Label :for="`ape-${u.username}`">{{ t("account.password") }}</Label>
            <Input :id="`ape-${u.username}`" v-model="form.adminPassword" type="password" autocomplete="current-password" />
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="busy || !form.adminPassword"><LoaderCircle v-if="busy" class="animate-spin" />{{ t("users.enable") }}</Button>
            <Button type="button" size="sm" variant="ghost" @click="close">{{ t("common.cancel") }}</Button>
          </div>
        </form>

        <!-- Reset password -->
        <form v-else-if="open === u.username && mode === 'password'" class="space-y-3 border-t pt-3" @submit.prevent="resetPassword(u)">
          <p class="text-sm text-muted-foreground">{{ t("users.resetHelp") }}</p>
          <div class="grid gap-3 sm:grid-cols-2">
            <div class="space-y-1.5">
              <Label :for="`np-${u.username}`">{{ t("users.newPassword") }}</Label>
              <Input :id="`np-${u.username}`" v-model="form.password" type="password" autocomplete="new-password" />
            </div>
            <div class="space-y-1.5">
              <Label :for="`npc-${u.username}`">{{ t("users.repeatPassword") }}</Label>
              <Input :id="`npc-${u.username}`" v-model="form.confirm" type="password" autocomplete="new-password" />
            </div>
          </div>
          <p v-if="form.password && [...form.password].length < policy.password_min_length" class="text-xs text-destructive">
            {{ t("wizard.tooShort", { n: policy.password_min_length }) }}
          </p>
          <label class="flex items-center gap-2 text-sm"><input v-model="form.mustChange" type="checkbox" class="size-4 accent-primary" />{{ t("users.mustChangeLabel") }}</label>
          <div class="space-y-1.5">
            <Label :for="`ap2-${u.username}`">{{ t("account.password") }}</Label>
            <Input :id="`ap2-${u.username}`" v-model="form.adminPassword" type="password" autocomplete="current-password" />
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" :disabled="busy || !form.password || form.password !== form.confirm || !form.adminPassword">
              <LoaderCircle v-if="busy" class="animate-spin" />{{ t("users.resetPassword") }}
            </Button>
            <Button type="button" size="sm" variant="ghost" @click="close">{{ t("common.cancel") }}</Button>
          </div>
        </form>

        <!-- Hapus -->
        <form v-else-if="open === u.username && mode === 'delete'" class="space-y-3 border-t pt-3" @submit.prevent="remove(u)">
          <p class="rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-sm">{{ t("users.deleteHelp", { name: u.username }) }}</p>
          <div class="space-y-1.5">
            <Label :for="`cn-${u.username}`">{{ t("users.typeName") }}</Label>
            <Input :id="`cn-${u.username}`" v-model="form.confirmName" autocomplete="off" />
          </div>
          <div class="space-y-1.5">
            <Label :for="`ap3-${u.username}`">{{ t("account.password") }}</Label>
            <Input :id="`ap3-${u.username}`" v-model="form.adminPassword" type="password" autocomplete="current-password" />
          </div>
          <div class="flex gap-2">
            <Button type="submit" size="sm" variant="destructive" :disabled="busy || form.confirmName !== u.username || !form.adminPassword">
              <LoaderCircle v-if="busy" class="animate-spin" />{{ t("users.delete") }}
            </Button>
            <Button type="button" size="sm" variant="ghost" @click="close">{{ t("common.cancel") }}</Button>
          </div>
        </form>
        <p v-if="open === u.username && error" class="text-sm text-destructive" role="alert">{{ error }}</p>
      </li>
    </ul>

    <!-- Grup -->
    <template v-if="tab === 'groups' && groups">
      <form class="space-y-2 rounded-xl border p-3" @submit.prevent="createGroup">
        <h3 class="text-sm font-medium">{{ t("groups.new") }}</h3>
        <div class="grid gap-2 sm:grid-cols-[1fr_2fr_auto]">
          <Input v-model="newGroup.name" :placeholder="t('groups.name')" autocapitalize="off" spellcheck="false" />
          <Input v-model="newGroup.description" :placeholder="t('groups.description')" maxlength="128" />
          <Button type="submit" size="sm" :disabled="busy || !newGroup.name"><Plus />{{ t("groups.create") }}</Button>
        </div>
        <p class="text-xs text-muted-foreground">{{ t("groups.nameHelp") }}</p>
        <p v-if="!open && error" class="text-sm text-destructive" role="alert">{{ error }}</p>
      </form>
      <p v-if="!groups.length" class="text-sm text-muted-foreground">{{ t("groups.none") }}</p>
      <ul class="space-y-2">
        <li v-for="g in groups" :key="g.name" class="space-y-2 rounded-xl border p-3">
          <div class="flex flex-wrap items-start justify-between gap-2">
            <div class="flex items-start gap-3">
              <UsersRound class="mt-0.5 size-5 shrink-0 text-muted-foreground" />
              <div>
                <div class="font-medium">{{ g.name }}</div>
                <p v-if="g.description" class="text-xs text-muted-foreground">{{ g.description }}</p>
                <p class="text-xs text-muted-foreground">{{ t("groups.members") }}: {{ g.members.length ? g.members.join(", ") : "—" }}</p>
              </div>
            </div>
            <div v-if="!members[g.name]" class="flex gap-1">
              <Button size="sm" variant="outline" @click="editMembers(g)">{{ t("groups.editMembers") }}</Button>
              <Button v-if="deleting !== g.name" size="sm" variant="ghost" class="text-destructive hover:text-destructive" @click="deleting = g.name"><Trash2 /></Button>
              <Button v-else size="sm" variant="destructive" :disabled="busy" @click="deleteGroup(g)">{{ t("groups.confirmDelete") }}</Button>
            </div>
          </div>
          <form v-if="members[g.name]" class="space-y-2 border-t pt-2" @submit.prevent="saveMembers(g)">
            <div class="flex flex-wrap gap-3">
              <label v-for="u in users ?? []" :key="u.username" class="flex items-center gap-1.5 text-sm">
                <input
                  type="checkbox"
                  class="size-4 accent-primary"
                  :checked="members[g.name]!.includes(u.username)"
                  @change="toggleIn(members[g.name]!, u.username, ($event.target as HTMLInputElement).checked)"
                />{{ u.username }}
              </label>
            </div>
            <div class="flex gap-2">
              <Button type="submit" size="sm" :disabled="busy">{{ t("users.save") }}</Button>
              <Button type="button" size="sm" variant="ghost" @click="delete members[g.name]">{{ t("common.cancel") }}</Button>
            </div>
          </form>
        </li>
      </ul>
    </template>

    <Skeleton v-if="!users && !loadError" class="h-32 rounded-xl" />
  </div>
</template>
