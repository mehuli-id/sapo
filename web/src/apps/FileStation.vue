<script setup lang="ts">
// File Station (semua pengguna, ADR 0010): menjelajah shared folder yang
// boleh dibuka pengguna, unduh, unggah, buat folder, ganti nama, dan hapus.
// Izin ditegakkan sapod lewat akun Linux pengguna (sama dengan SMB). Versi
// sebelumnya (snapshot, ADR 0014) dijelajah baca saja dan bisa dipulihkan.
import { computed, onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import {
  ChevronRight,
  Download,
  File,
  Folder,
  FolderLock,
  FolderPlus,
  History,
  LoaderCircle,
  Pencil,
  RefreshCw,
  RotateCcw,
  Trash2,
  Upload,
} from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import type { FileEntry, FileShare } from "@/lib/api"
import { api, ApiError, fileDownloadURL, uploadFile } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { cn } from "@/lib/utils"

const { t, locale } = useI18n()

const shares = ref<FileShare[] | null>(null)
// Folder pribadi (Sapo Drive) tampil sebagai "Drive saya".
const label = (s: FileShare) => (s.home ? t("files.home") : s.name)
const share = ref<FileShare | null>(null)
const path = ref("")
const entries = ref<FileEntry[] | null>(null)
const truncated = ref(false)
const error = ref("")
const notice = ref("")
const busy = ref(false)

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`files.errors.${c}`) !== `files.errors.${c}`
    ? t(`files.errors.${c}`)
    : t(`errors.${c}`) !== `errors.${c}`
      ? t(`errors.${c}`, { message: e instanceof Error ? e.message : "" })
      : t("errors.generic")
}

onMounted(async () => {
  try {
    shares.value = await api.fileShares()
    if (shares.value.length === 1) open(shares.value[0]!, "")
  } catch (e) {
    error.value = message(e)
  }
})

async function load() {
  if (!share.value) return
  error.value = ""
  try {
    const res = await api.listFiles(share.value.name, path.value, version.value)
    // Folder dulu, lalu nama.
    entries.value = res.entries.sort((a, b) => Number(b.type === "dir") - Number(a.type === "dir") || a.name.localeCompare(b.name, locale.value))
    truncated.value = !!res.truncated
  } catch (e) {
    entries.value = []
    error.value = message(e)
  }
}

function open(s: FileShare, p: string) {
  if (share.value?.name !== s.name) {
    version.value = ""
    versions.value = null
  }
  share.value = s
  path.value = p
  entries.value = null
  editing.value = ""
  notice.value = ""
  load()
}

const crumbs = computed(() => {
  const parts = path.value ? path.value.split("/") : []
  return parts.map((name, i) => ({ name, path: parts.slice(0, i + 1).join("/") }))
})
const join = (name: string) => (path.value ? `${path.value}/${name}` : name)
const canWrite = computed(() => share.value?.access === "write")
// Mengubah isi hanya di versi sekarang; di versi lama hanya memulihkan.
const canEdit = computed(() => canWrite.value && !version.value)

// --- Versi sebelumnya ---
const versions = ref<{ name: string; kind: string; created: string }[] | null>(null)
const version = ref("")
const showVersions = ref(false)
async function toggleVersions() {
  showVersions.value = !showVersions.value
  if (showVersions.value && share.value) {
    try {
      versions.value = await api.fileVersions(share.value.name)
    } catch (e) {
      error.value = message(e)
    }
  }
}
function pickVersion(name: string) {
  version.value = name
  showVersions.value = false
  entries.value = null
  editing.value = ""
  notice.value = ""
  load()
}
const versionTime = computed(() => {
  const v = versions.value?.find((x) => x.name === version.value)
  return v ? when(v.created) : ""
})
const restoreForm = reactive({ overwrite: false })
const restore = (e: FileEntry) =>
  run(async () => {
    const res = await api.restoreFile(share.value!.name, join(e.name), version.value, e.type === "file" && restoreForm.overwrite)
    restoreForm.overwrite = false
    restored.value = res.skipped ? t("files.restoredSkipped", { name: res.name, n: res.skipped }) : t("files.restored", { name: res.name })
  }, "")
const restored = ref("")
const size = (b: number) => formatBytes(b, locale.value, 1000)
const when = (iso: string) => new Date(iso).toLocaleString(locale.value, { dateStyle: "medium", timeStyle: "short" })

async function run(fn: () => Promise<unknown>, done: string) {
  busy.value = true
  error.value = ""
  notice.value = ""
  restored.value = ""
  try {
    await fn()
    notice.value = done || restored.value
    editing.value = ""
    await load()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

// --- Buat folder, ganti nama, hapus ---
const editing = ref("") // "" | "mkdir" | "rename:<nama>" | "delete:<nama>"
const form = reactive({ name: "" })
function start(mode: string, name = "") {
  editing.value = mode
  form.name = name
  error.value = ""
}
const validName = computed(() => {
  const n = form.name.trim()
  return n !== "" && n !== "." && n !== ".." && !n.includes("/") && n.length <= 255
})
const mkdir = () => run(() => api.mkdir(share.value!.name, join(form.name.trim())), t("files.created", { name: form.name.trim() }))
const rename = (from: string) => run(() => api.renameFile(share.value!.name, join(from), join(form.name.trim())), t("files.renamed", { name: form.name.trim() }))
const remove = (name: string) => run(() => api.deleteFile(share.value!.name, join(name)), t("files.deleted", { name }))

// --- Unggah ---
const uploads = ref<{ name: string; size: number; loaded: number; error?: string }[]>([])
const input = ref<HTMLInputElement | null>(null)
const dragging = ref(false)
async function upload(files: FileList | File[]) {
  if (!share.value || !canWrite.value) return
  const list = Array.from(files)
  const s = share.value.name
  const dir = path.value
  const existing = new Set((entries.value ?? []).map((e) => e.name))
  for (const f of list) {
    const item = reactive({ name: f.name, size: f.size, loaded: 0, error: undefined as string | undefined })
    uploads.value.push(item)
    try {
      // File dengan nama yang sama hanya ditimpa bila pengguna setuju.
      const overwrite = existing.has(f.name) && window.confirm(t("files.overwrite", { name: f.name }))
      if (existing.has(f.name) && !overwrite) throw new ApiError(409, "file_exists", "", {})
      await uploadFile(s, dir, f, overwrite, (n) => (item.loaded = n))
      item.loaded = f.size
    } catch (e) {
      item.error = message(e)
    }
  }
  uploads.value = uploads.value.filter((u) => u.error)
  if (share.value?.name === s && path.value === dir) load()
}
function onDrop(e: DragEvent) {
  dragging.value = false
  if (e.dataTransfer?.files.length) upload(e.dataTransfer.files)
}
</script>

<template>
  <div class="flex h-full min-h-0 flex-col">
    <!-- Pilihan folder bersama -->
    <div v-if="!share" class="space-y-3 p-4">
      <p class="text-sm text-muted-foreground">{{ t("files.intro") }}</p>
      <p v-if="error" class="rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm" role="alert">{{ error }}</p>
      <p v-if="shares && !shares.length" class="rounded-xl border p-4 text-sm text-muted-foreground">{{ t("files.noShares") }}</p>
      <ul v-if="shares?.length" class="grid gap-2 sm:grid-cols-2">
        <li v-for="s in shares" :key="s.name">
          <button type="button" class="flex w-full items-start gap-3 rounded-xl border p-3 text-left hover:bg-accent" @click="open(s, '')">
            <FolderLock class="mt-0.5 size-5 shrink-0 text-muted-foreground" />
            <span class="min-w-0">
              <span class="block font-medium">{{ label(s) }}</span>
              <span class="block text-xs text-muted-foreground">{{ s.home ? t("files.homeHelp") : s.description || t(`files.access.${s.access}`) }}</span>
            </span>
          </button>
        </li>
      </ul>
      <Skeleton v-if="!shares && !error" class="h-24 rounded-xl" />
    </div>

    <template v-else>
      <!-- Jalur dan aksi -->
      <div class="flex flex-wrap items-center justify-between gap-2 border-b px-4 py-2">
        <nav class="flex min-w-0 flex-wrap items-center gap-1 text-sm" :aria-label="t('files.location')">
          <button v-if="(shares?.length ?? 0) > 1" type="button" class="text-muted-foreground hover:text-foreground" @click="share = null">{{ t("files.allShares") }}</button>
          <ChevronRight v-if="(shares?.length ?? 0) > 1" class="size-3.5 text-muted-foreground" />
          <button type="button" class="font-medium hover:underline" @click="open(share, '')">{{ label(share) }}</button>
          <template v-for="c in crumbs" :key="c.path">
            <ChevronRight class="size-3.5 text-muted-foreground" />
            <button type="button" class="max-w-40 truncate hover:underline" @click="open(share, c.path)">{{ c.name }}</button>
          </template>
        </nav>
        <div class="flex gap-1">
          <Button size="sm" variant="ghost" :aria-label="t('storage.refresh')" @click="load"><RefreshCw /></Button>
          <Button size="sm" variant="outline" :aria-expanded="showVersions" @click="toggleVersions"><History />{{ t("files.versions") }}</Button>
          <template v-if="canEdit">
            <Button size="sm" variant="outline" @click="start('mkdir')"><FolderPlus />{{ t("files.newFolder") }}</Button>
            <Button size="sm" @click="input?.click()"><Upload />{{ t("files.upload") }}</Button>
            <input ref="input" type="file" multiple class="hidden" @change="upload(($event.target as HTMLInputElement).files ?? []); ($event.target as HTMLInputElement).value = ''" />
          </template>
        </div>
      </div>

      <div
        :class="cn('min-h-0 flex-1 space-y-3 overflow-y-auto p-4', dragging && 'bg-primary/5 ring-2 ring-primary ring-inset')"
        @dragover.prevent="dragging = canEdit"
        @dragleave="dragging = false"
        @drop.prevent="onDrop"
      >
        <div v-if="showVersions" class="space-y-2 rounded-lg border p-3">
          <p class="text-xs text-muted-foreground">{{ t("files.versionsHelp") }}</p>
          <ul v-if="versions?.length" class="max-h-56 space-y-0.5 overflow-y-auto">
            <li v-if="version">
              <button type="button" class="w-full rounded px-2 py-1 text-left text-sm hover:bg-accent" @click="pickVersion('')">{{ t("files.currentVersion") }}</button>
            </li>
            <li v-for="v in versions" :key="v.name">
              <button type="button" :class="cn('w-full rounded px-2 py-1 text-left text-sm hover:bg-accent', v.name === version && 'bg-accent')" @click="pickVersion(v.name)">
                {{ when(v.created) }} <span class="text-xs text-muted-foreground">· {{ t(`snapshots.kind.${v.kind}`) }}</span>
              </button>
            </li>
          </ul>
          <p v-else-if="versions" class="text-sm text-muted-foreground">{{ t("files.noVersions") }}</p>
        </div>
        <div v-if="version" class="flex flex-wrap items-center gap-2 rounded-lg border border-primary/40 bg-primary/5 p-2.5 text-sm">
          <History class="size-4 shrink-0" />
          <span class="flex-1">{{ t("files.viewingVersion", { when: versionTime }) }}</span>
          <Button size="sm" variant="ghost" @click="pickVersion('')">{{ t("files.backToCurrent") }}</Button>
        </div>
        <p v-if="!canWrite" class="rounded-lg bg-muted p-2.5 text-xs">{{ t("files.readOnly") }}</p>
        <p v-if="notice" class="rounded-lg border border-emerald-500/40 bg-emerald-500/5 p-2.5 text-sm">{{ notice }}</p>
        <p v-if="error" class="rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-sm" role="alert">{{ error }}</p>
        <p v-if="truncated" class="rounded-lg bg-muted p-2.5 text-xs">{{ t("files.truncated") }}</p>

        <ul v-if="uploads.length" class="space-y-1 text-xs">
          <li v-for="(u, i) in uploads" :key="i" class="flex items-center gap-2">
            <LoaderCircle v-if="!u.error" class="size-3.5 animate-spin" />
            <span class="truncate">{{ u.name }}</span>
            <span v-if="u.error" class="text-destructive">{{ u.error }}</span>
            <span v-else class="text-muted-foreground tabular-nums">{{ u.size ? Math.round((100 * u.loaded) / u.size) : 100 }}%</span>
          </li>
        </ul>

        <form v-if="editing === 'mkdir'" class="flex gap-2" @submit.prevent="validName && mkdir()">
          <Input v-model="form.name" :placeholder="t('files.folderName')" autofocus class="max-w-xs" />
          <Button type="submit" size="sm" :disabled="busy || !validName">{{ t("files.create") }}</Button>
          <Button type="button" size="sm" variant="ghost" @click="editing = ''">{{ t("common.cancel") }}</Button>
        </form>

        <table v-if="entries?.length" class="w-full text-sm">
          <thead class="text-left text-xs text-muted-foreground">
            <tr>
              <th class="py-1.5 font-normal">{{ t("files.name") }}</th>
              <th class="hidden py-1.5 text-right font-normal sm:table-cell">{{ t("files.size") }}</th>
              <th class="hidden py-1.5 pl-4 font-normal md:table-cell">{{ t("files.modified") }}</th>
              <th class="w-0" />
            </tr>
          </thead>
          <tbody>
            <template v-for="e in entries" :key="e.name">
              <tr class="group border-t">
                <td class="max-w-0 py-1.5">
                  <button v-if="e.type === 'dir'" type="button" class="flex w-full items-center gap-2 text-left hover:underline" @click="open(share, join(e.name))">
                    <Folder class="size-4 shrink-0 text-muted-foreground" /><span class="truncate">{{ e.name }}</span>
                  </button>
                  <a v-else-if="e.type === 'file'" :href="fileDownloadURL(share.name, join(e.name), version)" download class="flex items-center gap-2 hover:underline">
                    <File class="size-4 shrink-0 text-muted-foreground" /><span class="truncate">{{ e.name }}</span>
                  </a>
                  <span v-else class="flex items-center gap-2 text-muted-foreground" :title="t('files.special')">
                    <File class="size-4 shrink-0" /><span class="truncate">{{ e.name }}</span>
                  </span>
                </td>
                <td class="hidden py-1.5 text-right text-muted-foreground tabular-nums sm:table-cell">{{ e.type === "file" ? size(e.size) : "" }}</td>
                <td class="hidden py-1.5 pl-4 text-muted-foreground md:table-cell">{{ when(e.mtime) }}</td>
                <td class="py-1.5 pl-2">
                  <div class="flex justify-end gap-0.5 opacity-60 group-hover:opacity-100">
                    <a v-if="e.type === 'file'" :href="fileDownloadURL(share.name, join(e.name), version)" download :aria-label="t('files.download')" class="rounded p-1 hover:bg-accent">
                      <Download class="size-4" />
                    </a>
                    <button
                      v-if="version && canWrite && (e.type === 'file' || e.type === 'dir')"
                      type="button"
                      :aria-label="t('files.restore')"
                      class="rounded p-1 hover:bg-accent"
                      @click="start(`restore:${e.name}`)"
                    >
                      <RotateCcw class="size-4" />
                    </button>
                    <template v-if="canEdit">
                      <button type="button" :aria-label="t('files.rename')" class="rounded p-1 hover:bg-accent" @click="start(`rename:${e.name}`, e.name)"><Pencil class="size-4" /></button>
                      <button type="button" :aria-label="t('files.delete')" class="rounded p-1 text-destructive hover:bg-accent" @click="start(`delete:${e.name}`)"><Trash2 class="size-4" /></button>
                    </template>
                  </div>
                </td>
              </tr>
              <tr v-if="editing === `rename:${e.name}`">
                <td colspan="4" class="pb-2">
                  <form class="flex gap-2" @submit.prevent="validName && rename(e.name)">
                    <Input v-model="form.name" autofocus class="max-w-xs" />
                    <Button type="submit" size="sm" :disabled="busy || !validName || form.name.trim() === e.name">{{ t("files.rename") }}</Button>
                    <Button type="button" size="sm" variant="ghost" @click="editing = ''">{{ t("common.cancel") }}</Button>
                  </form>
                </td>
              </tr>
              <tr v-if="editing === `restore:${e.name}`">
                <td colspan="4" class="pb-2">
                  <div class="space-y-2 rounded-lg border p-2.5 text-sm">
                    <p>{{ t(e.type === "dir" ? "files.restoreFolder" : "files.restoreFile", { name: e.name }) }}</p>
                    <label v-if="e.type === 'file'" class="flex items-center gap-2 text-xs">
                      <input v-model="restoreForm.overwrite" type="checkbox" class="size-4 accent-primary" />{{ t("files.restoreOverwrite") }}
                    </label>
                    <div class="flex gap-2">
                      <Button size="sm" :disabled="busy" @click="restore(e)"><LoaderCircle v-if="busy" class="animate-spin" /><RotateCcw v-else />{{ t("files.restore") }}</Button>
                      <Button size="sm" variant="ghost" @click="editing = ''">{{ t("common.cancel") }}</Button>
                    </div>
                  </div>
                </td>
              </tr>
              <tr v-if="editing === `delete:${e.name}`">
                <td colspan="4" class="pb-2">
                  <div class="flex flex-wrap items-center gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-sm">
                    <span class="flex-1">{{ t(e.type === "dir" ? "files.deleteFolder" : "files.deleteFile", { name: e.name }) }}</span>
                    <Button size="sm" variant="destructive" :disabled="busy" @click="remove(e.name)"><LoaderCircle v-if="busy" class="animate-spin" />{{ t("files.delete") }}</Button>
                    <Button size="sm" variant="ghost" @click="editing = ''">{{ t("common.cancel") }}</Button>
                  </div>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
        <p v-else-if="entries && !error" class="py-8 text-center text-sm text-muted-foreground">{{ canEdit ? t("files.emptyDrop") : t("files.empty") }}</p>
        <Skeleton v-if="!entries" class="h-40 rounded-xl" />
      </div>
    </template>
  </div>
</template>
