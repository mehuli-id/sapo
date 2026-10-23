<script setup lang="ts">
// Mode Ringkas (dan tampilan HP): sidebar + satu halaman konten.
import { computed, ref } from "vue"
import { useI18n } from "vue-i18n"
import { Menu, X } from "@lucide/vue"
import { apps, componentFor, findApp, iconFor } from "@/apps/registry"
import SapoLogo from "@/components/SapoLogo.vue"
import { useShell } from "@/composables/useShell"
import { cn } from "@/lib/utils"
import ShellMenu from "./ShellMenu.vue"

const { t } = useI18n()
const { branding, isSmall } = useShell()
const activeId = ref(apps.find((a) => a.status === "ready")?.id ?? apps[0]?.id ?? "")
const sidebarOpen = ref(false)
const active = computed(() => findApp(activeId.value))

function select(id: string) {
  activeId.value = id
  sidebarOpen.value = false
}
</script>

<template>
  <div class="flex h-full overflow-hidden">
    <div
      v-if="isSmall && sidebarOpen"
      class="fixed inset-0 z-30 bg-black/40"
      aria-hidden="true"
      @click="sidebarOpen = false"
    />
    <aside
      :class="
        cn(
          'z-40 flex w-64 shrink-0 flex-col border-r bg-muted/40',
          isSmall && 'fixed inset-y-0 left-0 bg-background shadow-xl transition-transform',
          isSmall && !sidebarOpen && '-translate-x-full',
        )
      "
    >
      <div class="flex h-14 items-center gap-2 px-4">
        <SapoLogo :size="22" class="text-primary" />
        <span class="flex-1 truncate font-semibold">{{ branding?.product.full_name ?? "Sapo" }}</span>
        <button v-if="isSmall" type="button" class="grid size-8 place-items-center rounded-md hover:bg-accent" :aria-label="t('common.close')" @click="sidebarOpen = false">
          <X class="size-4" />
        </button>
      </div>
      <nav class="flex-1 space-y-0.5 overflow-y-auto px-2 pb-4" :aria-label="t('shell.allApps')">
        <button
          v-for="app in apps"
          :key="app.id"
          type="button"
          :class="
            cn(
              'flex h-9 w-full items-center gap-3 rounded-md px-3 text-left text-sm hover:bg-accent',
              activeId === app.id && 'bg-accent font-medium text-accent-foreground',
              app.status === 'planned' && 'text-muted-foreground',
            )
          "
          :aria-current="activeId === app.id ? 'page' : undefined"
          @click="select(app.id)"
        >
          <component :is="iconFor(app.icon)" class="size-4 shrink-0" :class="activeId === app.id && 'text-primary'" />
          <span class="truncate">{{ t(`apps.${app.id}.title`) }}</span>
        </button>
      </nav>
      <p class="border-t px-4 py-3 text-[11px] leading-snug text-muted-foreground">{{ t("shell.devBanner") }}</p>
    </aside>

    <div class="flex min-w-0 flex-1 flex-col">
      <header class="flex h-14 shrink-0 items-center gap-2 border-b px-4">
        <button v-if="isSmall" type="button" class="grid size-9 place-items-center rounded-md hover:bg-accent" :aria-label="t('shell.mainMenu')" @click="sidebarOpen = true">
          <Menu class="size-5" />
        </button>
        <h1 class="flex-1 truncate font-semibold">{{ active ? t(`apps.${active.id}.title`) : "" }}</h1>
        <ShellMenu />
      </header>
      <main class="min-h-0 flex-1 overflow-y-auto">
        <div class="mx-auto max-w-4xl">
          <component :is="componentFor(active)" v-if="active" :key="active.id" :app="active" />
        </div>
      </main>
    </div>
  </div>
</template>
