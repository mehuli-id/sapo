<script setup lang="ts">
// Grid aplikasi dengan pencarian. Dipakai sebagai menu utama (mode Desktop).
import { computed, onMounted, ref } from "vue"
import { useI18n } from "vue-i18n"
import { Search } from "@lucide/vue"
import type { AppDef } from "@/apps/registry"
import { apps, iconFor } from "@/apps/registry"
import { Badge } from "@/components/ui/badge"

const emit = defineEmits<{ open: [app: AppDef] }>()
const { t } = useI18n()
const query = ref("")
const input = ref<HTMLInputElement | null>(null)

const results = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return apps
  return apps.filter((a) =>
    [t(`apps.${a.id}.title`), t(`apps.${a.id}.description`), ...(a.keywords ?? [])].some((s) =>
      s.toLowerCase().includes(q),
    ),
  )
})

function onEnter() {
  const first = results.value[0]
  if (first) emit("open", first)
}

onMounted(() => input.value?.focus())
</script>

<template>
  <div class="flex flex-col gap-4">
    <label class="relative block">
      <Search class="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground" />
      <input
        ref="input"
        v-model="query"
        type="search"
        :placeholder="t('common.search')"
        class="h-10 w-full rounded-md border bg-background pr-3 pl-9 text-sm outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
        @keydown.enter="onEnter"
      />
    </label>
    <p class="text-xs font-medium text-muted-foreground">{{ t("shell.allApps") }}</p>
    <div v-if="results.length" class="grid grid-cols-3 gap-2 sm:grid-cols-4">
      <button
        v-for="app in results"
        :key="app.id"
        type="button"
        class="group flex flex-col items-center gap-2 rounded-lg p-3 text-center hover:bg-accent focus-visible:bg-accent focus-visible:outline-none"
        @click="emit('open', app)"
      >
        <span
          class="grid size-11 place-items-center rounded-xl bg-primary/10 text-primary transition-colors group-hover:bg-primary group-hover:text-primary-foreground"
        >
          <component :is="iconFor(app.icon)" class="size-5" />
        </span>
        <span class="text-xs leading-tight font-medium">{{ t(`apps.${app.id}.title`) }}</span>
        <Badge v-if="app.status === 'planned'" variant="outline" class="h-4 px-1.5 text-[10px] font-normal">
          {{ t("common.comingSoon") }}
        </Badge>
      </button>
    </div>
    <p v-else class="py-6 text-center text-sm text-muted-foreground">{{ t("common.noResults") }}</p>
  </div>
</template>
