<script setup lang="ts">
import { useI18n } from "vue-i18n"
import { ExternalLink } from "@lucide/vue"
import SapoLogo from "@/components/SapoLogo.vue"
import { Separator } from "@/components/ui/separator"
import { useShell } from "@/composables/useShell"

const { t, locale } = useI18n()
const { branding } = useShell()
</script>

<template>
  <div class="flex flex-col items-center gap-4 p-6 text-center">
    <span class="grid size-16 place-items-center rounded-2xl bg-primary/10 text-primary">
      <SapoLogo :size="40" />
    </span>
    <div>
      <h3 class="text-lg font-semibold">{{ branding?.product.full_name ?? "Sapo" }}</h3>
      <p class="text-sm text-muted-foreground">{{ branding?.product.tagline?.[locale] }}</p>
    </div>
    <Separator />
    <p class="text-sm text-muted-foreground">
      {{ t("about.license", { vendor: branding?.product.vendor ?? "" }) }}
    </p>
    <div class="flex gap-4 text-sm">
      <a
        v-if="branding?.product.website"
        :href="branding.product.website"
        target="_blank"
        rel="noopener noreferrer"
        class="inline-flex items-center gap-1 text-primary hover:underline"
      >
        {{ t("about.website") }} <ExternalLink class="size-3.5" />
      </a>
    </div>
  </div>
</template>
