<script setup lang="ts">
import { computed, onMounted, ref } from "vue"
import { useI18n } from "vue-i18n"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import type { Feature } from "@/lib/api"
import { api, ApiError } from "@/lib/api"

const { t, te, locale } = useI18n()
const features = ref<Feature[] | null>(null)
const error = ref<string | null>(null)

async function load() {
  try {
    features.value = await api.features()
    error.value = null
  } catch (e) {
    error.value = e instanceof ApiError && e.code === "agent_unavailable" ? t("shell.agentDown") : t("common.error")
  }
}
onMounted(load)

// Urutan dan nama grup mengikuti data manifest; label grup dari file terjemahan.
const groups = computed(() => {
  const map = new Map<string, Feature[]>()
  for (const f of features.value ?? []) {
    if (!map.has(f.group)) map.set(f.group, [])
    map.get(f.group)!.push(f)
  }
  return [...map.entries()]
    .map(([id, items]) => ({
      id,
      label: te(`features.groups.${id}`) ? t(`features.groups.${id}`) : id,
      items,
    }))
    // Urutan: grup berisi fitur "always", lalu "setup", lalu sisanya urut abjad.
    .sort((a, b) => rank(a.items) - rank(b.items) || a.label.localeCompare(b.label))
})

const order = { always: 0, setup: 1, optional: 2 } as const
const rank = (items: Feature[]) => Math.min(...items.map((f) => order[f.activation] ?? 2))

const name = (f: Feature) => f.name[locale.value] ?? f.name.en ?? f.id
</script>

<template>
  <div class="space-y-5 p-4">
    <p class="text-sm text-muted-foreground">{{ t("features.intro") }}</p>

    <div v-if="error" class="flex items-center justify-between gap-3 rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm">
      <span>{{ error }}</span>
      <Button size="sm" variant="outline" @click="load">{{ t("common.retry") }}</Button>
    </div>

    <section v-for="g in groups" :key="g.id" class="space-y-2">
      <h3 class="text-xs font-semibold tracking-wide text-muted-foreground uppercase">{{ g.label }}</h3>
      <ul class="divide-y rounded-xl border">
        <li v-for="f in g.items" :key="f.id" class="space-y-2 p-3">
          <div class="flex items-center justify-between gap-2">
            <span class="font-medium">{{ name(f) }}</span>
            <Badge :variant="f.activation === 'optional' ? 'outline' : 'secondary'">
              {{ t(`features.activation.${f.activation}`) }}
            </Badge>
          </div>
          <div class="flex flex-wrap items-center gap-1.5 text-xs">
            <span class="text-muted-foreground">{{ t("features.packages") }}:</span>
            <code v-for="p in f.packages" :key="p" class="rounded bg-muted px-1.5 py-0.5 font-mono">{{ p }}</code>
          </div>
          <div v-if="f.services.length" class="flex flex-wrap items-center gap-1.5 text-xs">
            <span class="text-muted-foreground">{{ t("features.services") }}:</span>
            <code v-for="s in f.services" :key="s" class="rounded bg-muted px-1.5 py-0.5 font-mono">{{ s }}</code>
          </div>
        </li>
      </ul>
    </section>

    <div v-if="!features && !error" class="space-y-2">
      <Skeleton v-for="i in 3" :key="i" class="h-20 rounded-xl" />
    </div>
  </div>
</template>
