<script setup lang="ts">
// Mode Desktop: desktop berjendela ala DSM dengan taskbar dan menu utama.
import { computed, onMounted, onUnmounted, ref } from "vue"
import { useI18n } from "vue-i18n"
import { onClickOutside, onKeyStroke, useElementSize } from "@vueuse/core"
import type { AppDef } from "@/apps/registry"
import { apps, findApp, iconFor } from "@/apps/registry"
import SapoLogo from "@/components/SapoLogo.vue"
import { Kbd } from "@/components/ui/kbd"
import { useShell } from "@/composables/useShell"
import { useWindows } from "@/composables/useWindows"
import { cn } from "@/lib/utils"
import AppLauncher from "./AppLauncher.vue"
import AppWindow from "./AppWindow.vue"
import ShellMenu from "./ShellMenu.vue"

const { t, locale } = useI18n()
const { branding } = useShell()
const { state, focused, open, toggleMinimize } = useWindows()

const areaEl = ref<HTMLElement | null>(null)
const { width, height } = useElementSize(areaEl)
const area = computed(() => ({ width: width.value, height: height.value }))

const menuOpen = ref(false)
const menuEl = ref<HTMLElement | null>(null)
const menuButton = ref<HTMLElement | null>(null)
onClickOutside(menuEl, () => (menuOpen.value = false), { ignore: [menuButton] })
onKeyStroke("Escape", () => (menuOpen.value = false))
onKeyStroke(
  (e) => e.key.toLowerCase() === "k" && (e.ctrlKey || e.metaKey),
  (e) => {
    e.preventDefault()
    menuOpen.value = !menuOpen.value
  },
)

function launch(app: AppDef) {
  menuOpen.value = false
  open(app, area.value)
}

const desktopApps = computed(() => apps)
const now = ref(new Date())
let clockTimer: number | undefined
onMounted(() => (clockTimer = window.setInterval(() => (now.value = new Date()), 15_000)))
onUnmounted(() => window.clearInterval(clockTimer))
const clock = computed(() => now.value.toLocaleTimeString(locale.value, { hour: "2-digit", minute: "2-digit" }))
const date = computed(() => now.value.toLocaleDateString(locale.value, { weekday: "short", day: "numeric", month: "short" }))
const isMac = typeof navigator !== "undefined" && /Mac/i.test(navigator.platform)
</script>

<template>
  <div class="flex h-full flex-col overflow-hidden">
    <main ref="areaEl" class="desktop-wallpaper relative min-h-0 flex-1 overflow-hidden">
      <p
        class="pointer-events-none absolute top-3 left-1/2 -translate-x-1/2 rounded-full bg-black/25 px-3 py-1 text-[11px] text-white/85 backdrop-blur"
      >
        {{ t("shell.devBanner") }}
      </p>

      <!-- Ikon desktop. Tinggi baris harus memuat label dua baris (misalnya
           "Storage Manager"): padding 16 + ikon 48 + jarak 6 + teks 2×15 = 100px. -->
      <nav
        class="absolute inset-y-0 left-0 grid content-start gap-1 p-3"
        style="grid-template-rows: repeat(auto-fill, 108px); grid-auto-flow: column"
        :aria-label="t('shell.allApps')"
      >
        <button
          v-for="app in desktopApps"
          :key="app.id"
          type="button"
          class="flex w-[88px] flex-col items-center gap-1.5 rounded-lg p-2 text-white hover:bg-white/10 focus-visible:bg-white/15 focus-visible:outline-none"
          @click="launch(app)"
        >
          <span class="grid size-12 place-items-center rounded-2xl bg-white/90 text-primary shadow-md dark:bg-white/15 dark:text-white">
            <component :is="iconFor(app.icon)" class="size-6" />
          </span>
          <span class="line-clamp-2 text-center text-xs leading-tight [text-shadow:0_1px_2px_rgb(0_0_0/0.6)]">
            {{ t(`apps.${app.id}.title`) }}
          </span>
        </button>
      </nav>

      <!-- Jendela aplikasi -->
      <template v-for="win in state.windows" :key="win.appId">
        <AppWindow
          v-if="findApp(win.appId)"
          :win="win"
          :app="findApp(win.appId)!"
          :area="area"
          :focused="focused === win.appId"
        />
      </template>

      <!-- Menu utama -->
      <Transition
        enter-active-class="transition duration-150 ease-out"
        enter-from-class="translate-y-2 opacity-0"
        leave-active-class="transition duration-100 ease-in"
        leave-to-class="translate-y-2 opacity-0"
      >
        <div
          v-if="menuOpen"
          ref="menuEl"
          class="absolute bottom-2 left-2 z-[9999] w-[440px] max-w-[calc(100%-1rem)] rounded-xl border bg-popover p-4 text-popover-foreground shadow-2xl"
          role="dialog"
          :aria-label="t('shell.mainMenu')"
        >
          <div class="mb-4 flex items-center gap-2">
            <SapoLogo :size="22" class="text-primary" />
            <span class="font-semibold">{{ branding?.product.full_name ?? "Sapo" }}</span>
          </div>
          <AppLauncher @open="launch" />
        </div>
      </Transition>
    </main>

    <!-- Taskbar -->
    <footer class="flex h-12 shrink-0 items-center gap-1 border-t bg-taskbar px-2 backdrop-blur-xl">
      <button
        ref="menuButton"
        type="button"
        :class="cn('flex h-9 items-center gap-2 rounded-md px-2.5 hover:bg-accent', menuOpen && 'bg-accent')"
        :aria-label="t('shell.mainMenu')"
        :aria-expanded="menuOpen"
        @click="menuOpen = !menuOpen"
      >
        <SapoLogo :size="20" class="text-primary" />
        <Kbd class="hidden lg:inline-flex">{{ isMac ? "⌘" : "Ctrl" }} K</Kbd>
      </button>
      <div class="mx-1 h-6 w-px bg-border" />
      <div class="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto">
        <button
          v-for="win in state.windows"
          :key="win.appId"
          type="button"
          :class="
            cn(
              'flex h-9 max-w-48 shrink-0 items-center gap-2 rounded-md px-3 text-sm hover:bg-accent',
              focused === win.appId && !win.minimized && 'bg-accent font-medium',
              win.minimized && 'text-muted-foreground',
            )
          "
          @click="toggleMinimize(win.appId)"
        >
          <component :is="iconFor(findApp(win.appId)?.icon ?? '')" class="size-4 shrink-0 text-primary" />
          <span class="truncate">{{ t(`apps.${win.appId}.title`) }}</span>
        </button>
      </div>
      <div class="flex flex-col items-end px-2 leading-tight">
        <span class="text-sm font-medium tabular-nums">{{ clock }}</span>
        <span class="text-[11px] text-muted-foreground">{{ date }}</span>
      </div>
      <ShellMenu />
    </footer>
  </div>
</template>
