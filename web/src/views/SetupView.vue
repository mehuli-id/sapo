<script setup lang="ts">
import { onMounted, ref } from "vue"
import { useI18n } from "vue-i18n"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { useSession } from "@/composables/useSession"
import type { Wizard } from "@/lib/api"
import { api, wizardActions } from "@/lib/api"
import { pick } from "@/lib/localized"
import WizardRunner from "@/wizard/WizardRunner.vue"
import AuthLayout from "./AuthLayout.vue"

const { t, locale } = useI18n()
const { policy, refresh } = useSession()
const wizard = ref<Wizard | null>(null)
const failed = ref(false)

async function load() {
  failed.value = false
  try {
    wizard.value = await api.wizard("setup")
  } catch {
    failed.value = true
  }
}

async function submit(values: Record<string, string>) {
  const action = wizardActions[wizard.value!.action]
  if (!action) throw new Error(`tindakan wizard tidak dikenal: ${wizard.value!.action}`)
  return action(values)
}

onMounted(load)
</script>

<template>
  <AuthLayout wide>
    <template v-if="wizard">
      <h1 class="mb-6 text-sm font-medium text-muted-foreground">{{ pick(wizard.title, locale) }}</h1>
      <WizardRunner :wizard="wizard" :policy="policy" :submit="submit" @done="refresh" />
    </template>
    <div v-else-if="failed" class="space-y-3 text-center">
      <p class="text-sm">{{ t("errors.generic") }}</p>
      <Button variant="outline" @click="load">{{ t("common.retry") }}</Button>
    </div>
    <div v-else class="space-y-3">
      <Skeleton class="h-7 w-full" />
      <Skeleton class="h-40 w-full" />
    </div>
  </AuthLayout>
</template>
