<script setup lang="ts">
// Isian disk-select: memilih disk untuk pool. Disk yang sedang dipakai atau
// tidak diizinkan tampil tetapi tidak bisa dipilih, beserta alasannya.
import { computed } from "vue"
import { useI18n } from "vue-i18n"
import { Check, HardDrive } from "@lucide/vue"
import { Badge } from "@/components/ui/badge"
import type { Disk } from "@/lib/api"
import { splitList } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { ineligibleReason } from "@/lib/pool"
import { cn } from "@/lib/utils"

const props = defineProps<{ modelValue: string; disks: Disk[]; allowRemovable: boolean; inputId: string; invalid?: boolean }>()
const emit = defineEmits<{ "update:modelValue": [string] }>()
const { t, locale } = useI18n()

const selected = computed(() => new Set(splitList(props.modelValue)))
const size = (b: number) => formatBytes(b, locale.value, 1000)

function toggle(d: Disk) {
  if (ineligibleReason(d, props.allowRemovable)) return
  const next = new Set(selected.value)
  if (next.has(d.name)) next.delete(d.name)
  else next.add(d.name)
  // Urutan mengikuti daftar disk, bukan urutan klik.
  emit("update:modelValue", props.disks.filter((x) => next.has(x.name)).map((x) => x.name).join(","))
}

const picked = computed(() => props.disks.filter((d) => selected.value.has(d.name)))
const mixedSizes = computed(() => new Set(picked.value.map((d) => d.size_bytes)).size > 1)
const smallest = computed(() => Math.min(...picked.value.map((d) => d.size_bytes)))
</script>

<template>
  <div class="space-y-2">
    <ul :id="inputId" role="group" :aria-invalid="invalid || undefined" class="space-y-2">
      <li v-for="d in disks" :key="d.name">
        <button
          type="button"
          role="checkbox"
          :aria-checked="selected.has(d.name)"
          :disabled="!!ineligibleReason(d, allowRemovable)"
          :class="
            cn(
              'flex w-full items-start gap-3 rounded-lg border px-3 py-2.5 text-left text-sm hover:bg-accent disabled:cursor-not-allowed disabled:opacity-60 disabled:hover:bg-transparent',
              selected.has(d.name) && 'border-primary ring-1 ring-primary',
            )
          "
          @click="toggle(d)"
        >
          <span :class="cn('mt-0.5 grid size-4 shrink-0 place-items-center rounded border', selected.has(d.name) && 'border-primary bg-primary text-primary-foreground')">
            <Check v-if="selected.has(d.name)" class="size-3" />
          </span>
          <HardDrive class="mt-0.5 size-4 shrink-0 text-muted-foreground" />
          <span class="min-w-0 flex-1">
            <span class="block truncate font-medium">{{ d.model || d.name }}</span>
            <span class="block text-xs text-muted-foreground">
              {{ size(d.size_bytes) }} · <code class="font-mono">{{ d.name }}</code><template v-if="d.serial"> · {{ d.serial }}</template>
            </span>
            <span v-if="ineligibleReason(d, allowRemovable)" class="block text-xs text-muted-foreground">{{ t(ineligibleReason(d, allowRemovable)) }}</span>
          </span>
          <Badge v-if="d.usage === 'data'" variant="outline" class="border-amber-500/60 text-amber-700 dark:text-amber-400">{{ t("storage.usage.data") }}</Badge>
          <Badge v-else-if="d.usage === 'free' && !ineligibleReason(d, allowRemovable)" variant="secondary">{{ t("storage.usage.free") }}</Badge>
        </button>
      </li>
    </ul>
    <p v-if="!disks.length" class="text-sm text-muted-foreground">{{ t("storage.empty") }}</p>
    <p v-if="mixedSizes" class="rounded-lg border border-amber-500/40 bg-amber-500/5 p-2.5 text-xs">
      {{ t("pool.mixedSizes", { size: size(smallest) }) }}
    </p>
  </div>
</template>
