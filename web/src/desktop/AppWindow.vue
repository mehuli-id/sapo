<script setup lang="ts">
import { computed } from "vue"
import { useI18n } from "vue-i18n"
import { Copy, Minus, Square, X } from "@lucide/vue"
import type { AppDef } from "@/apps/registry"
import { componentFor, iconFor } from "@/apps/registry"
import type { WinState } from "@/composables/useWindows"
import { clamp, useWindows } from "@/composables/useWindows"
import { cn } from "@/lib/utils"

const props = defineProps<{
  win: WinState
  app: AppDef
  area: { width: number; height: number }
  focused: boolean
}>()

const { t } = useI18n()
const { close, focus, toggleMinimize, remember } = useWindows()

const MIN_W = 320
const MIN_H = 220

const style = computed(() =>
  props.win.maximized
    ? { left: "0px", top: "0px", width: `${props.area.width}px`, height: `${props.area.height}px`, zIndex: props.win.z }
    : {
        left: `${props.win.x}px`,
        top: `${props.win.y}px`,
        width: `${props.win.width}px`,
        height: `${props.win.height}px`,
        zIndex: props.win.z,
      },
)

function startDrag(e: PointerEvent) {
  if (e.button !== 0 || props.win.maximized) return
  if ((e.target as HTMLElement).closest("button")) return
  focus(props.win.appId)
  const startX = e.clientX
  const startY = e.clientY
  const ox = props.win.x
  const oy = props.win.y
  const el = e.currentTarget as HTMLElement
  el.setPointerCapture(e.pointerId)
  const move = (ev: PointerEvent) => {
    props.win.x = clamp(ox + ev.clientX - startX, -props.win.width + 120, props.area.width - 120)
    props.win.y = clamp(oy + ev.clientY - startY, 0, props.area.height - 40)
  }
  const up = (ev: PointerEvent) => {
    el.removeEventListener("pointermove", move)
    el.removeEventListener("pointerup", up)
    snap(ev.clientX)
    remember(props.win)
  }
  el.addEventListener("pointermove", move)
  el.addEventListener("pointerup", up)
}

// Snap: dilepas di tepi kiri/kanan layar → jendela menjadi setengah layar.
function snap(pointerX: number) {
  const edge = 6
  const half = Math.round(props.area.width / 2)
  if (pointerX <= edge) {
    Object.assign(props.win, { x: 0, y: 0, width: half, height: props.area.height })
  } else if (pointerX >= props.area.width - edge) {
    Object.assign(props.win, { x: props.area.width - half, y: 0, width: half, height: props.area.height })
  }
}

function startResize(e: PointerEvent) {
  if (e.button !== 0 || props.win.maximized) return
  e.stopPropagation()
  focus(props.win.appId)
  const startX = e.clientX
  const startY = e.clientY
  const ow = props.win.width
  const oh = props.win.height
  const el = e.currentTarget as HTMLElement
  el.setPointerCapture(e.pointerId)
  const move = (ev: PointerEvent) => {
    props.win.width = clamp(ow + ev.clientX - startX, MIN_W, props.area.width - props.win.x)
    props.win.height = clamp(oh + ev.clientY - startY, MIN_H, props.area.height - props.win.y)
  }
  const up = () => {
    el.removeEventListener("pointermove", move)
    el.removeEventListener("pointerup", up)
    remember(props.win)
  }
  el.addEventListener("pointermove", move)
  el.addEventListener("pointerup", up)
}

function toggleMaximize() {
  props.win.maximized = !props.win.maximized
  focus(props.win.appId)
}

const Icon = computed(() => iconFor(props.app.icon))
const Body = computed(() => componentFor(props.app))
</script>

<template>
  <section
    v-show="!win.minimized"
    role="dialog"
    :aria-label="t(`apps.${app.id}.title`)"
    :style="style"
    :class="
      cn(
        'absolute flex flex-col overflow-hidden border bg-card text-card-foreground shadow-2xl',
        win.maximized ? 'rounded-none' : 'rounded-xl',
        focused ? 'ring-1 ring-primary/30' : 'opacity-[0.97]',
      )
    "
    @pointerdown="focus(win.appId)"
  >
    <header
      class="flex h-10 shrink-0 select-none items-center gap-2 border-b bg-muted/60 px-3"
      :class="win.maximized ? 'cursor-default' : 'cursor-grab active:cursor-grabbing'"
      @pointerdown="startDrag"
      @dblclick="toggleMaximize"
    >
      <component :is="Icon" class="size-4 text-primary" />
      <h2 class="min-w-0 flex-1 truncate text-sm font-medium">{{ t(`apps.${app.id}.title`) }}</h2>
      <div class="flex items-center gap-0.5">
        <button
          type="button"
          class="grid size-7 place-items-center rounded-md text-muted-foreground hover:bg-accent hover:text-foreground"
          :aria-label="t('common.minimize')"
          :title="t('common.minimize')"
          @click="toggleMinimize(win.appId)"
        >
          <Minus class="size-4" />
        </button>
        <button
          type="button"
          class="grid size-7 place-items-center rounded-md text-muted-foreground hover:bg-accent hover:text-foreground"
          :aria-label="win.maximized ? t('common.restore') : t('common.maximize')"
          :title="win.maximized ? t('common.restore') : t('common.maximize')"
          @click="toggleMaximize"
        >
          <Copy v-if="win.maximized" class="size-3.5" />
          <Square v-else class="size-3.5" />
        </button>
        <button
          type="button"
          class="grid size-7 place-items-center rounded-md text-muted-foreground hover:bg-destructive hover:text-white"
          :aria-label="t('common.close')"
          :title="t('common.close')"
          @click="close(win.appId)"
        >
          <X class="size-4" />
        </button>
      </div>
    </header>
    <div class="min-h-0 flex-1 overflow-auto">
      <component :is="Body" :app="app" />
    </div>
    <div
      v-if="!win.maximized"
      class="absolute right-0 bottom-0 size-4 cursor-se-resize"
      aria-hidden="true"
      @pointerdown="startResize"
    />
  </section>
</template>
