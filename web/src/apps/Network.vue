<script setup lang="ts">
// Pengaturan jaringan (hanya admin, ADR 0009): nama host, DHCP atau alamat
// tetap per antarmuka, dan DNS. Setiap perubahan harus dikonfirmasi; bila
// panel tidak bisa dibuka lagi (misalnya alamat salah), sapod mengembalikan
// pengaturan lama sendiri.
import { computed, onBeforeUnmount, onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, LoaderCircle, Network, RefreshCw, TriangleAlert } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import type { NetworkApplyResult, NetworkConfig, NetworkStatus } from "@/lib/api"
import { api, ApiError } from "@/lib/api"
import { cn } from "@/lib/utils"

const { t } = useI18n()

const status = ref<NetworkStatus | null>(null)
const loadError = ref("")
const notice = ref("")
const error = ref("")
const busy = ref(false)
const form = reactive({ hostname: "", interfaces: [] as NetworkConfig["interfaces"], dns: "", adminPassword: "" })

function message(e: unknown) {
  const c = e instanceof ApiError ? e.code : "generic"
  return t(`errors.${c}`) !== `errors.${c}` ? t(`errors.${c}`, { message: e instanceof Error ? e.message : "" }) : t("errors.generic")
}

async function load() {
  try {
    const s = await api.networkStatus()
    status.value = s
    loadError.value = ""
    const cfg = s.config
    form.hostname = cfg?.hostname ?? s.hostname
    form.interfaces = s.interfaces.map((l) => {
      const c = cfg?.interfaces.find((i) => i.name === l.name)
      return { name: l.name, mode: c?.mode ?? "dhcp", address: c?.address ?? "", gateway: c?.gateway ?? "" }
    })
    form.dns = (cfg?.dns ?? []).join("\n")
    form.adminPassword = ""
  } catch (e) {
    loadError.value = message(e)
  }
}
onMounted(load)

// DNS diisi sendiri bila tidak ada antarmuka DHCP.
const allStatic = computed(() => form.interfaces.some((i) => i.mode === "static") && form.interfaces.every((i) => i.mode !== "dhcp"))
const link = (name: string) => status.value?.interfaces.find((l) => l.name === name)

function config(): NetworkConfig {
  return {
    hostname: form.hostname.trim().toLowerCase(),
    interfaces: form.interfaces.map((i) =>
      i.mode === "static" ? { name: i.name, mode: "static", address: i.address?.trim(), gateway: i.gateway?.trim() || undefined } : { name: i.name, mode: i.mode },
    ),
    dns: allStatic.value ? form.dns.split(/[\s,]+/).filter(Boolean) : [],
  }
}

// --- Terapkan dan konfirmasi ---
const change = ref<(NetworkApplyResult & { config: NetworkConfig }) | null>(null)
const remaining = ref(0)
let timer: ReturnType<typeof setInterval> | undefined
onBeforeUnmount(() => clearInterval(timer))

// Alamat panel baru untuk konfirmasi: alamat tetap yang baru dan nama host
// (bila berbeda dari alamat yang sedang dipakai browser).
const confirmLinks = computed(() => {
  if (!change.value) return []
  const port = window.location.port ? `:${window.location.port}` : ""
  const hosts = change.value.config.interfaces.filter((i) => i.mode === "static" && i.address).map((i) => i.address!.split("/")[0]!)
  hosts.push(`${change.value.config.hostname}.local`)
  return [...new Set(hosts)]
    .filter((h) => h !== window.location.hostname)
    .map((h) => `https://${h}${port}/#network-confirm=${change.value!.token}`)
})

async function apply() {
  busy.value = true
  error.value = ""
  notice.value = ""
  try {
    const cfg = config()
    const res = await api.applyNetwork(cfg, form.adminPassword)
    form.adminPassword = ""
    change.value = { ...res, config: cfg }
    startConfirming()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

// Setelah pengaturan baru berlaku, panel mencoba mengonfirmasi dari alamat
// ini. Bila alamat NAS berubah, permintaan gagal sampai admin membuka alamat
// baru (tautan di layar) atau waktunya habis.
function startConfirming() {
  clearInterval(timer)
  let trying = false
  const tick = async () => {
    const c = change.value
    if (!c) return
    remaining.value = Math.max(0, Math.round((Date.parse(c.confirm_by) - Date.now()) / 1000))
    if (remaining.value === 0) {
      // Batas waktu sebenarnya dihitung sapod sejak pengaturan selesai
      // dipasang; bila masih menunggu, batas yang baru dipakai.
      try {
        const s = await api.networkStatus()
        if (s.pending && Date.parse(s.pending.confirm_by) > Date.now()) {
          c.confirm_by = s.pending.confirm_by
          return
        }
      } catch {
        // tidak terjangkau: anggap sudah dikembalikan
      }
      clearInterval(timer)
      change.value = null
      error.value = t("network.reverted")
      setTimeout(load, 5000)
      return
    }
    if (trying || Date.now() < Date.parse(c.applies_at) + 3000) return
    trying = true
    try {
      await api.confirmNetwork(c.token)
      clearInterval(timer)
      change.value = null
      notice.value = t("network.saved")
      await load()
    } catch (e) {
      // Belum berlaku (busy) atau alamat ini tidak terjangkau: coba lagi.
      if (e instanceof ApiError && e.code === "not_found") {
        clearInterval(timer)
        change.value = null
        error.value = t("network.reverted")
      }
    } finally {
      trying = false
    }
  }
  timer = setInterval(tick, 2000)
  tick()
}

async function cancel() {
  if (!change.value) return
  try {
    await api.cancelNetwork(change.value.token)
  } catch {
    // tidak terjangkau: pengaturan lama tetap dikembalikan saat waktunya habis
  }
  clearInterval(timer)
  change.value = null
  notice.value = t("network.cancelled")
  setTimeout(load, 5000)
}
</script>

<template>
  <div class="space-y-4 p-4">
    <div class="flex items-start justify-between gap-3">
      <p class="text-sm text-muted-foreground">{{ t("network.intro") }}</p>
      <Button size="sm" variant="ghost" :disabled="!!change" @click="load"><RefreshCw />{{ t("storage.refresh") }}</Button>
    </div>
    <p v-if="notice" class="flex items-center gap-2 rounded-lg border border-emerald-500/40 bg-emerald-500/5 p-3 text-sm">
      <CircleCheck class="size-4 shrink-0 text-emerald-600" />{{ notice }}
    </p>
    <p v-if="loadError" class="rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm">{{ loadError }}</p>
    <p v-if="error" class="rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm" role="alert">{{ error }}</p>

    <!-- Menunggu konfirmasi -->
    <div v-if="change" class="space-y-3 rounded-xl border border-amber-500/40 bg-amber-500/5 p-4 text-sm">
      <p class="flex items-center gap-2 font-medium"><LoaderCircle class="size-4 shrink-0 animate-spin" />{{ t("network.confirming", { seconds: remaining }) }}</p>
      <p class="text-muted-foreground">{{ t("network.confirmHelp") }}</p>
      <ul v-if="confirmLinks.length" class="space-y-1">
        <li v-for="l in confirmLinks" :key="l">
          <a :href="l" target="_blank" rel="noopener" class="font-mono text-xs break-all underline">{{ l.split("/#")[0] }}</a>
        </li>
      </ul>
      <Button size="sm" variant="outline" @click="cancel">{{ t("network.cancel") }}</Button>
    </div>
    <p v-else-if="status?.pending" class="flex items-center gap-2 rounded-lg border border-amber-500/40 bg-amber-500/5 p-3 text-sm">
      <TriangleAlert class="size-4 shrink-0 text-amber-600" />{{ t("network.pendingElsewhere") }}
    </p>

    <template v-if="status">
      <p v-if="!status.config" class="rounded-lg bg-muted p-3 text-sm">{{ t("network.unmanaged") }}</p>
      <form class="space-y-4" @submit.prevent="apply">
        <div class="space-y-1.5">
          <Label for="net-host">{{ t("network.hostname") }}</Label>
          <Input id="net-host" v-model="form.hostname" autocomplete="off" spellcheck="false" />
          <p class="text-xs text-muted-foreground">{{ t("network.hostnameHelp", { name: form.hostname || "sapo" }) }}</p>
        </div>

        <div v-for="i in form.interfaces" :key="i.name" class="space-y-3 rounded-xl border p-4">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <div class="flex items-center gap-2">
              <Network class="size-4 text-muted-foreground" />
              <span class="font-medium">{{ i.name }}</span>
              <span class="text-xs text-muted-foreground">{{ link(i.name)?.mac }}</span>
            </div>
            <span :class="cn('text-xs', link(i.name)?.up ? 'text-emerald-600' : 'text-muted-foreground')">
              {{ link(i.name)?.up ? t("network.up", { speed: link(i.name)?.speed_mbps ?? "?" }) : t("network.down") }}
            </span>
          </div>
          <p class="text-xs text-muted-foreground">{{ t("network.current") }}: {{ link(i.name)?.addresses.join(", ") || "—" }}</p>
          <div class="flex gap-2" role="radiogroup">
            <Button
              v-for="m in ['dhcp', 'static', 'off'] as const"
              :key="m"
              type="button"
              size="sm"
              role="radio"
              :aria-checked="i.mode === m"
              :variant="i.mode === m ? 'default' : 'outline'"
              @click="i.mode = m"
            >
              {{ t(`network.mode.${m}`) }}
            </Button>
          </div>
          <div v-if="i.mode === 'static'" class="grid gap-3 sm:grid-cols-2">
            <div class="space-y-1.5">
              <Label :for="`net-a-${i.name}`">{{ t("network.address") }}</Label>
              <Input :id="`net-a-${i.name}`" v-model="i.address" placeholder="192.168.1.10/24" autocomplete="off" class="font-mono" />
            </div>
            <div class="space-y-1.5">
              <Label :for="`net-g-${i.name}`">{{ t("network.gateway") }}</Label>
              <Input :id="`net-g-${i.name}`" v-model="i.gateway" placeholder="192.168.1.1" autocomplete="off" class="font-mono" />
            </div>
          </div>
        </div>

        <div v-if="allStatic" class="space-y-1.5">
          <Label for="net-dns">{{ t("network.dns") }}</Label>
          <textarea
            id="net-dns"
            v-model="form.dns"
            rows="2"
            placeholder="192.168.1.1"
            class="w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-sm shadow-xs focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
          />
          <p class="text-xs text-muted-foreground">{{ t("network.dnsHelp") }}</p>
        </div>
        <p v-else class="text-xs text-muted-foreground">{{ t("network.dnsDhcp", { dns: status.dns.join(", ") || "—" }) }}</p>

        <p class="rounded-lg border border-amber-500/40 bg-amber-500/5 p-3 text-xs">{{ t("network.warning") }}</p>
        <div class="space-y-1.5">
          <Label for="net-pw">{{ t("account.password") }}</Label>
          <Input id="net-pw" v-model="form.adminPassword" type="password" autocomplete="current-password" />
        </div>
        <Button type="submit" size="sm" :disabled="busy || !!change || !!status.pending || !form.adminPassword">
          <LoaderCircle v-if="busy" class="animate-spin" />{{ t("network.apply") }}
        </Button>
      </form>
    </template>
    <Skeleton v-if="!status && !loadError" class="h-48 rounded-xl" />
  </div>
</template>
