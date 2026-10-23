<script setup lang="ts">
// Menu pengguna: akun, mode tampilan, tema, dan bahasa. Dipakai di kedua mode.
import { ref } from "vue"
import { useI18n } from "vue-i18n"
import { Languages, LogOut, Monitor, Moon, PanelsTopLeft, Sun, UserRound } from "@lucide/vue"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { useSession } from "@/composables/useSession"
import type { DisplayMode } from "@/composables/useShell"
import { useShell } from "@/composables/useShell"
import { languages, setLocale } from "@/i18n"
import { useColorScheme } from "@/lib/theme"

defineProps<{ triggerClass?: string }>()

const { t, locale } = useI18n()
const { mode, setMode, isSmall } = useShell()
const { status, logout } = useSession()
const scheme = useColorScheme()
const theme = ref(scheme.read())

function onTheme(v: unknown) {
  theme.value = v as "light" | "dark" | "system"
  scheme.set(theme.value)
}
</script>

<template>
  <DropdownMenu>
    <DropdownMenuTrigger
      :class="triggerClass ?? 'grid size-9 place-items-center rounded-md hover:bg-accent'"
      :aria-label="t('shell.mode')"
    >
      <UserRound class="size-4" />
    </DropdownMenuTrigger>
    <DropdownMenuContent align="end" side="top" class="w-56">
      <DropdownMenuLabel v-if="status?.user" class="truncate font-normal">
        {{ t("shell.signedInAs", { name: status.user.username }) }}
      </DropdownMenuLabel>
      <DropdownMenuSeparator v-if="status?.user" />
      <template v-if="!isSmall">
        <DropdownMenuLabel class="flex items-center gap-2 text-xs text-muted-foreground">
          <PanelsTopLeft class="size-3.5" />{{ t("shell.mode") }}
        </DropdownMenuLabel>
        <DropdownMenuRadioGroup :model-value="mode" @update:model-value="(v) => setMode(v as DisplayMode)">
          <DropdownMenuRadioItem value="desktop">{{ t("shell.modeDesktop") }}</DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="ringkas">{{ t("shell.modeCompact") }}</DropdownMenuRadioItem>
        </DropdownMenuRadioGroup>
        <DropdownMenuSeparator />
      </template>
      <DropdownMenuLabel class="flex items-center gap-2 text-xs text-muted-foreground">
        <Sun class="size-3.5" />{{ t("shell.theme") }}
      </DropdownMenuLabel>
      <DropdownMenuRadioGroup :model-value="theme" @update:model-value="onTheme">
        <DropdownMenuRadioItem value="light"><Sun />{{ t("shell.themeLight") }}</DropdownMenuRadioItem>
        <DropdownMenuRadioItem value="dark"><Moon />{{ t("shell.themeDark") }}</DropdownMenuRadioItem>
        <DropdownMenuRadioItem value="system"><Monitor />{{ t("shell.themeSystem") }}</DropdownMenuRadioItem>
      </DropdownMenuRadioGroup>
      <DropdownMenuSeparator />
      <DropdownMenuLabel class="flex items-center gap-2 text-xs text-muted-foreground">
        <Languages class="size-3.5" />{{ t("shell.language") }}
      </DropdownMenuLabel>
      <DropdownMenuRadioGroup :model-value="locale" @update:model-value="(v) => setLocale(String(v))">
        <DropdownMenuRadioItem v-for="l in languages" :key="l.code" :value="l.code">{{ l.label }}</DropdownMenuRadioItem>
      </DropdownMenuRadioGroup>
      <DropdownMenuSeparator />
      <DropdownMenuItem @select="logout"><LogOut />{{ t("shell.logout") }}</DropdownMenuItem>
    </DropdownMenuContent>
  </DropdownMenu>
</template>
