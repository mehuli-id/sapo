<script setup lang="ts">
// Isian pool-layout: susunan RAID untuk disk yang sudah dipilih. Susunan
// yang tidak cocok dengan jumlah disk tampil nonaktif beserta syaratnya.
import { computed, watch } from "vue"
import { useI18n } from "vue-i18n"
import { Check } from "@lucide/vue"
import { Badge } from "@/components/ui/badge"
import type { Disk, PoolLayout } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { faultTolerance, fits, recommended, usableBytes } from "@/lib/pool"
import { cn } from "@/lib/utils"

const props = defineProps<{ modelValue: string; layouts: PoolLayout[]; disks: Disk[]; inputId: string; invalid?: boolean }>()
const emit = defineEmits<{ "update:modelValue": [string] }>()
const { t, locale } = useI18n()

const n = computed(() => props.disks.length)
const best = computed(() => recommended(props.layouts, n.value))
const sizes = computed(() => props.disks.map((d) => d.size_bytes))

// Pilihan yang tidak lagi cocok (jumlah disk berubah) diganti saran.
watch(
  [n, () => props.layouts],
  () => {
    const cur = props.layouts.find((l) => l.id === props.modelValue)
    if (!cur || !fits(cur, n.value)) emit("update:modelValue", best.value)
  },
  { immediate: true },
)
</script>

<template>
  <div :id="inputId" role="radiogroup" :aria-invalid="invalid || undefined" class="space-y-2">
    <button
      v-for="l in layouts"
      :key="l.id"
      type="button"
      role="radio"
      :aria-checked="modelValue === l.id"
      :disabled="!fits(l, n)"
      :class="
        cn(
          'flex w-full items-start gap-3 rounded-lg border px-3 py-2.5 text-left text-sm hover:bg-accent disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-transparent',
          modelValue === l.id && 'border-primary ring-1 ring-primary',
        )
      "
      @click="emit('update:modelValue', l.id)"
    >
      <span class="min-w-0 flex-1 space-y-0.5">
        <span class="flex flex-wrap items-center gap-2 font-medium">
          {{ t(`pool.layout.${l.id}.name`) }}
          <Badge v-if="l.id === best" variant="secondary">{{ t("pool.recommended") }}</Badge>
        </span>
        <span class="block text-xs text-muted-foreground">{{ t(`pool.layout.${l.id}.desc`) }}</span>
        <span v-if="fits(l, n)" class="block text-xs">
          {{ t("pool.capacityAbout", { size: formatBytes(usableBytes(l.id, sizes), locale, 1000) }) }} ·
          <span :class="faultTolerance(l.id, n) === 0 && 'font-medium text-destructive'">{{ t("pool.tolerates", { n: faultTolerance(l.id, n) }, faultTolerance(l.id, n)) }}</span>
        </span>
        <span v-else class="block text-xs text-muted-foreground">
          {{ l.min_disks === l.max_disks ? t("pool.needsExactly", { n: l.min_disks }) : t("pool.needsRange", { min: l.min_disks, max: l.max_disks }) }}
        </span>
      </span>
      <Check v-if="modelValue === l.id" class="size-4 shrink-0 text-primary" />
    </button>
  </div>
</template>
