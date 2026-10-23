import { ref } from "vue"
import type { AuthPolicy, AuthStatus } from "@/lib/api"
import { api, setUnauthenticatedHandler } from "@/lib/api"

const status = ref<AuthStatus | null>(null)
const policy = ref<AuthPolicy>({ password_min_length: 10, username_pattern: "^[a-z][a-z0-9._-]{2,31}$" })

async function refresh() {
  try {
    status.value = await api.authStatus()
  } catch {
    status.value = { setup_required: false, authenticated: false }
  }
}

setUnauthenticatedHandler(() => {
  if (status.value?.authenticated) status.value = { ...status.value, authenticated: false, user: undefined }
})

export function useSession() {
  async function logout() {
    try {
      await api.logout()
    } finally {
      await refresh()
    }
  }
  return { status, policy, refresh, logout }
}
