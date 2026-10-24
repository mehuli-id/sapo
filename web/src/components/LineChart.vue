<script setup lang="ts">
// Grafik garis kecil untuk Resource Monitor: satu sumbu, garis 2px, grid
// samar, garis bantu vertikal dengan tooltip semua seri di titik terdekat,
// legenda untuk dua seri atau lebih, dan tabel nilai terakhir (dibaca
// pembaca layar dan tanpa hover).
import { computed, onBeforeUnmount, onMounted, ref } from "vue"

export interface Series {
  name: string
  // Variabel CSS warna seri, misalnya "var(--viz-1)".
  color: string
  values: number[]
}

const props = defineProps<{
  title: string
  times: number[] // milidetik
  series: Series[]
  format: (v: number) => string
  // Batas atas tetap (misalnya 100 untuk persen); tanpa ini dari data.
  max?: number
}>()

const H = 140
const PAD = { top: 8, right: 8, bottom: 18, left: 8 }
const root = ref<HTMLElement | null>(null)
const width = ref(480)
let ro: ResizeObserver | undefined
onMounted(() => {
  ro = new ResizeObserver((e) => (width.value = Math.max(200, e[0]!.contentRect.width)))
  if (root.value) ro.observe(root.value)
})
onBeforeUnmount(() => ro?.disconnect())

// Batas atas dibulatkan ke angka "rapi" agar garis grid mudah dibaca.
const top = computed(() => {
  if (props.max) return props.max
  const m = Math.max(0, ...props.series.flatMap((s) => s.values))
  if (m <= 0) return 1
  const p = 10 ** Math.floor(Math.log10(m))
  return ([1, 2, 5, 10].find((k) => k * p >= m) ?? 10) * p
})
const n = computed(() => props.times.length)
const x = (i: number) => PAD.left + (n.value <= 1 ? 0 : (i / (n.value - 1)) * (width.value - PAD.left - PAD.right))
const y = (v: number) => PAD.top + (1 - Math.min(v, top.value) / top.value) * (H - PAD.top - PAD.bottom)
const path = (vals: number[]) => vals.map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join("")
const grid = computed(() => [0, 0.5, 1].map((f) => ({ y: y(top.value * f), label: props.format(top.value * f) })))

const hover = ref<number | null>(null)
function move(e: PointerEvent) {
  if (n.value < 2) return
  const r = (e.currentTarget as SVGElement).getBoundingClientRect()
  const f = (e.clientX - r.left - PAD.left) / (r.width - PAD.left - PAD.right)
  hover.value = Math.max(0, Math.min(n.value - 1, Math.round(f * (n.value - 1))))
}
const clock = (ms: number) => new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })
const last = (s: Series) => (s.values.length ? props.format(s.values[s.values.length - 1]!) : "—")
</script>

<template>
  <figure ref="root" class="space-y-2">
    <figcaption class="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
      <span class="text-sm font-medium">{{ title }}</span>
      <!-- Legenda (dua seri atau lebih) sekaligus nilai terakhir. -->
      <span class="flex flex-wrap gap-x-4 text-xs text-muted-foreground">
        <span v-for="s in series" :key="s.name" class="flex items-center gap-1.5">
          <svg v-if="series.length > 1" width="14" height="2" aria-hidden="true"><line x1="0" y1="1" x2="14" y2="1" :stroke="s.color" stroke-width="2" stroke-linecap="round" /></svg>
          <span v-if="series.length > 1">{{ s.name }}</span>
          <span class="font-medium text-foreground tabular-nums">{{ last(s) }}</span>
        </span>
      </span>
    </figcaption>
    <div class="relative">
      <svg
        :width="width"
        :height="H"
        :viewBox="`0 0 ${width} ${H}`"
        class="block touch-none"
        role="img"
        :aria-label="title"
        @pointermove="move"
        @pointerleave="hover = null"
      >
        <g class="text-border">
          <line v-for="g in grid" :key="g.y" :x1="PAD.left" :x2="width - PAD.right" :y1="g.y" :y2="g.y" stroke="currentColor" stroke-width="1" />
        </g>
        <g class="fill-muted-foreground stroke-card text-[10px]" stroke-width="3" paint-order="stroke" stroke-linejoin="round">
          <text v-for="g in grid.slice(1)" :key="g.label" :x="width - PAD.right" :y="g.y + 11" text-anchor="end">{{ g.label }}</text>
          <text v-if="n" :x="PAD.left" :y="H - 4">{{ clock(times[0]!) }}</text>
          <text v-if="n > 1" :x="width - PAD.right" :y="H - 4" text-anchor="end">{{ clock(times[n - 1]!) }}</text>
        </g>
        <path v-for="s in series" :key="s.name" :d="path(s.values)" fill="none" :stroke="s.color" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
        <g v-if="hover !== null">
          <line :x1="x(hover)" :x2="x(hover)" :y1="PAD.top" :y2="H - PAD.bottom" class="stroke-muted-foreground" stroke-width="1" />
          <circle v-for="s in series" :key="s.name" :cx="x(hover)" :cy="y(s.values[hover] ?? 0)" r="4" :fill="s.color" class="stroke-card" stroke-width="2" />
        </g>
      </svg>
      <div
        v-if="hover !== null"
        class="pointer-events-none absolute top-1 z-10 min-w-32 rounded-md border bg-popover px-2.5 py-1.5 text-xs shadow-md"
        :style="x(hover) > width / 2 ? { right: `${width - x(hover) + 8}px` } : { left: `${x(hover) + 8}px` }"
      >
        <p class="mb-1 text-muted-foreground tabular-nums">{{ clock(times[hover]!) }}</p>
        <p v-for="s in series" :key="s.name" class="flex items-center gap-1.5">
          <svg width="10" height="2" aria-hidden="true"><line x1="0" y1="1" x2="10" y2="1" :stroke="s.color" stroke-width="2" /></svg>
          <span class="font-medium tabular-nums">{{ format(s.values[hover] ?? 0) }}</span>
          <span v-if="series.length > 1" class="text-muted-foreground">{{ s.name }}</span>
        </p>
      </div>
    </div>
  </figure>
</template>
