<script setup lang="ts">
// Penanda aplikasi yang sudah ada di rancangan tapi belum dibangun.
import { useI18n } from "vue-i18n"
import type { AppDef } from "@/apps/registry"
import { iconFor } from "@/apps/registry"
import { Badge } from "@/components/ui/badge"

defineProps<{ app: AppDef }>()
const { t } = useI18n()
</script>

<template>
  <div class="flex h-full min-h-60 flex-col items-center justify-center gap-3 p-6 text-center">
    <span class="grid size-14 place-items-center rounded-2xl bg-muted text-muted-foreground">
      <component :is="iconFor(app.icon)" class="size-7" />
    </span>
    <h3 class="font-semibold">{{ t(`apps.${app.id}.title`) }}</h3>
    <p class="max-w-xs text-sm text-muted-foreground">{{ t(`apps.${app.id}.description`) }}</p>
    <Badge variant="outline">{{ app.phase ? t("common.phase", { n: app.phase }) : t("common.comingSoon") }}</Badge>
  </div>
</template>
