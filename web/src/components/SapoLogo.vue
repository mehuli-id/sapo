<script setup lang="ts">
// Logo sementara Sapo: atap lumbung (sapo page) bergaya garis.
// Logo final mengikuti panduan brand di docs/brand/.
defineProps<{ size?: number }>()
</script>

<template>
  <svg :width="size ?? 24" :height="size ?? 24" viewBox="0 0 64 64" fill="none" aria-hidden="true">
    <path
      d="M5 31c9 2 18-1 27-14 9 13 18 16 27 14l-3-9c-9 1-17-3-24-15-7 12-15 16-24 15l-3 9Z"
      fill="currentColor"
    />
    <rect x="17" y="33" width="30" height="21" rx="2.5" fill="currentColor" opacity="0.55" />
    <path d="M22 40h20M22 46h20" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" opacity="0.9" />
  </svg>
</template>
