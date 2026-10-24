<script setup lang="ts">
// Izin shared folder per pengguna dan grup: tanpa akses, baca, atau baca &
// tulis. Nilai: { "user:budi": "write", "group:keluarga": "read" }.
import { useI18n } from "vue-i18n"
import { UserRound, UsersRound } from "@lucide/vue"
import type { ShareAccess, UserAccount, UserGroup } from "@/lib/api"
import { cn } from "@/lib/utils"

const props = defineProps<{ modelValue: Record<string, ShareAccess>; users: UserAccount[]; groups: UserGroup[] }>()
const emit = defineEmits<{ "update:modelValue": [Record<string, ShareAccess>] }>()
const { t } = useI18n()

const levels = ["none", "read", "write"] as const

function level(key: string) {
  return props.modelValue[key] ?? "none"
}

function set(key: string, l: (typeof levels)[number]) {
  const next = { ...props.modelValue }
  if (l === "none") delete next[key]
  else next[key] = l
  emit("update:modelValue", next)
}

const rows = () => [
  ...props.groups.map((g) => ({ key: `group:${g.name}`, name: g.name, group: true, note: t("permissions.members", { n: g.members.length }) })),
  ...props.users.map((u) => ({ key: `user:${u.username}`, name: u.username, group: false, note: u.role === "admin" ? t("users.role.admin") : "" })),
]
</script>

<template>
  <ul class="divide-y rounded-lg border text-sm">
    <li v-for="r in rows()" :key="r.key" class="flex flex-wrap items-center justify-between gap-2 px-3 py-2">
      <span class="flex min-w-0 items-center gap-2">
        <UsersRound v-if="r.group" class="size-4 shrink-0 text-muted-foreground" />
        <UserRound v-else class="size-4 shrink-0 text-muted-foreground" />
        <span class="truncate font-medium">{{ r.name }}</span>
        <span v-if="r.note" class="text-xs text-muted-foreground">{{ r.note }}</span>
      </span>
      <span class="flex gap-1 rounded-md bg-muted p-0.5" role="radiogroup" :aria-label="r.name">
        <button
          v-for="l in levels"
          :key="l"
          type="button"
          role="radio"
          :aria-checked="level(r.key) === l"
          :class="cn('rounded px-2 py-0.5 text-xs', level(r.key) === l ? 'bg-background font-medium shadow-sm' : 'text-muted-foreground')"
          @click="set(r.key, l)"
        >
          {{ t(`permissions.${l}`) }}
        </button>
      </span>
    </li>
    <li v-if="!users.length && !groups.length" class="px-3 py-2 text-muted-foreground">{{ t("permissions.nobody") }}</li>
  </ul>
</template>
