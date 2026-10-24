<script setup lang="ts">
// Isian erase-confirm: daftar disk yang akan dihapus (beserta isinya yang
// terbaca) dan kotak centang konfirmasi.
import { useI18n } from "vue-i18n"
import { TriangleAlert } from "@lucide/vue"
import type { Disk } from "@/lib/api"
import { formatBytes } from "@/lib/format"

defineProps<{ modelValue: string; disks: Disk[]; label: string; inputId: string }>()
const emit = defineEmits<{ "update:modelValue": [string] }>()
const { t, locale } = useI18n()
const size = (b: number) => formatBytes(b, locale.value, 1000)
</script>

<template>
  <div class="space-y-3">
    <div class="space-y-2 rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm">
      <p class="flex items-center gap-2 font-medium text-destructive"><TriangleAlert class="size-4 shrink-0" />{{ t("pool.eraseWarning") }}</p>
      <ul class="space-y-1.5">
        <li v-for="d in disks" :key="d.name">
          <span class="font-medium">{{ d.model || d.name }}</span>
          <span class="text-muted-foreground"> · {{ size(d.size_bytes) }} · <code class="font-mono">{{ d.name }}</code><template v-if="d.serial"> · {{ d.serial }}</template></span>
          <ul v-if="d.usage === 'data'" class="mt-0.5 ml-4 list-disc text-xs text-muted-foreground">
            <li v-for="p in d.partitions" :key="p.name">
              {{ p.name }} · {{ size(p.size_bytes) }}<template v-if="p.fstype"> · {{ p.fstype }}</template><template v-if="p.label"> · “{{ p.label }}”</template>
            </li>
            <li v-if="!d.partitions.length">{{ d.fstype || d.pttype }}</li>
          </ul>
        </li>
      </ul>
    </div>
    <label class="flex items-start gap-2 text-sm">
      <input
        :id="inputId"
        type="checkbox"
        class="mt-0.5 size-4 accent-destructive"
        :checked="modelValue === 'yes'"
        @change="emit('update:modelValue', ($event.target as HTMLInputElement).checked ? 'yes' : '')"
      />
      <span>{{ label }}</span>
    </label>
  </div>
</template>
