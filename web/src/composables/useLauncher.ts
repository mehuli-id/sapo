import { ref } from "vue"

// Permintaan membuka aplikasi dari dalam aplikasi lain (misalnya pintasan di
// Control Panel). Mode Desktop membuka jendelanya, mode Ringkas pindah
// halaman.
const request = ref<string | null>(null)

export function useLauncher() {
  return {
    request,
    launch(appId: string) {
      request.value = appId
    },
  }
}
