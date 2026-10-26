<script setup lang="ts">
// Pengaturan jaringan (hanya admin, ADR 0009): nama host, DHCP atau alamat
// tetap per antarmuka, jaringan Wi-Fi (ADR 0023), dan DNS. Setiap perubahan harus dikonfirmasi; bila
// panel tidak bisa dibuka lagi (misalnya alamat salah), sapod mengembalikan
// pengaturan lama sendiri.
import { computed, onBeforeUnmount, onMounted, reactive, ref } from "vue"
import { useI18n } from "vue-i18n"
import { CircleCheck, LoaderCircle, Network, RefreshCw, TriangleAlert, Wifi } from "@lucide/vue"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import type { NetworkApplyResult, NetworkConfig, NetworkInterfaceConfig, NetworkStatus, WiFiSeen } from "@/lib/api"
import { api, ApiError } from "@/lib/api"
import { cn } from "@/lib/utils"

const { t } = useI18n()

const status = ref<NetworkStatus | null>(null)
const loadError = ref("")
const notice = ref("")
const error = ref("")
const busy = ref(false)
// Per antarmuka: pengaturan, ditambah isian Wi-Fi (ssid, password, SSID
// yang tersimpan di sapod).
type FormInterface = NetworkInterfaceConfig & { wireless: boolean; ssid: string; password: string; savedSSID: string }
const form = reactive({ hostname: "", interfaces: [] as FormInterface[], dns: "", adminPassword: "" })

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
      const wireless = !!l.wireless
      return {
        name: l.name,
        // Wi-Fi yang belum diatur tidak dipakai.
        mode: c?.mode ?? (wireless ? "off" : "dhcp"),
        address: c?.address ?? "",
        gateway: c?.gateway ?? "",
        wireless,
        ssid: c?.wifi?.ssid ?? "",
        password: "",
        savedSSID: c?.wifi?.ssid ?? "",
      }
    })
    scans.value = {}
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
    interfaces: form.interfaces.map((i) => {
      const c: NetworkInterfaceConfig =
        i.mode === "static" ? { name: i.name, mode: "static", address: i.address?.trim(), gateway: i.gateway?.trim() || undefined } : { name: i.name, mode: i.mode }
      if (i.wireless && i.mode !== "off") c.wifi = { ssid: i.ssid, password: i.password || undefined }
      return c
    }),
    dns: allStatic.value ? form.dns.split(/[\s,]+/).filter(Boolean) : [],
  }
}

// --- Wi-Fi: pindai jaringan di sekitar ---
const scans = ref<Record<string, WiFiSeen[] | "busy" | string>>({})
async function scan(i: FormInterface) {
  scans.value[i.name] = "busy"
  try {
    scans.value[i.name] = await api.scanWiFi(i.name)
  } catch (e) {
    scans.value[i.name] = message(e)
  }
}
const signal = (dbm: number) => t(dbm >= -60 ? "network.wifi.strong" : dbm >= -72 ? "network.wifi.medium" : "network.wifi.weak")
// Password wajib kecuali SSID sama dengan yang tersimpan.
const wifiIncomplete = computed(() =>
  form.interfaces.some((i) => i.wireless && i.mode !== "off" && (!i.ssid || (i.ssid !== i.savedSSID && i.password.length < 8))),
)

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
              <Wifi v-if="i.wireless" class="size-4 text-muted-foreground" />
              <Network v-else class="size-4 text-muted-foreground" />
              <span class="font-medium">{{ i.name }}</span>
              <span class="text-xs text-muted-foreground">{{ link(i.name)?.mac }}</span>
            </div>
            <span v-if="i.wireless" :class="cn('text-xs', link(i.name)?.ssid ? 'text-emerald-600' : 'text-muted-foreground')">
              {{ link(i.name)?.ssid ? t("network.wifi.connected", { ssid: link(i.name)!.ssid!, signal: signal(link(i.name)!.signal_dbm ?? -100) }) : t("network.wifi.notConnected") }}
            </span>
            <span v-else :class="cn('text-xs', link(i.name)?.up ? 'text-emerald-600' : 'text-muted-foreground')">
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
          <div v-if="i.wireless && i.mode !== 'off'" class="space-y-3 rounded-lg bg-muted/50 p-3">
            <div class="flex flex-wrap items-center justify-between gap-2">
              <p class="text-xs text-muted-foreground">{{ t("network.wifi.help") }}</p>
              <Button type="button" size="sm" variant="outline" :disabled="scans[i.name] === 'busy'" @click="scan(i)">
                <LoaderCircle v-if="scans[i.name] === 'busy'" class="animate-spin" /><RefreshCw v-else />{{ t("network.wifi.scan") }}
              </Button>
            </div>
            <p v-if="typeof scans[i.name] === 'string' && scans[i.name] !== 'busy'" class="text-xs text-destructive">{{ scans[i.name] }}</p>
            <ul v-else-if="Array.isArray(scans[i.name])" class="max-h-48 divide-y overflow-y-auto rounded-md border bg-background text-sm">
              <li v-if="!(scans[i.name] as WiFiSeen[]).length" class="px-2 py-1.5 text-xs text-muted-foreground">{{ t("network.wifi.none") }}</li>
              <li v-for="w in scans[i.name] as WiFiSeen[]" :key="w.ssid">
                <button
                  type="button"
                  class="flex w-full items-center justify-between gap-2 px-2 py-1.5 text-left hover:bg-muted disabled:cursor-not-allowed disabled:opacity-60"
                  :class="i.ssid === w.ssid ? 'bg-primary/10' : ''"
                  :disabled="!w.secure"
                  @click="i.ssid = w.ssid"
                >
                  <span class="min-w-0 truncate">{{ w.ssid }}</span>
                  <span class="shrink-0 text-xs text-muted-foreground">{{ w.secure ? signal(w.signal_dbm) : t("network.wifi.unsupported") }}</span>
                </button>
              </li>
            </ul>
            <div class="grid gap-3 sm:grid-cols-2">
              <div class="space-y-1.5">
                <Label :for="`net-ssid-${i.name}`">{{ t("network.wifi.ssid") }}</Label>
                <Input :id="`net-ssid-${i.name}`" v-model="i.ssid" maxlength="32" autocomplete="off" spellcheck="false" />
              </div>
              <div class="space-y-1.5">
                <Label :for="`net-wpw-${i.name}`">{{ t("network.wifi.password") }}</Label>
                <Input
                  :id="`net-wpw-${i.name}`"
                  v-model="i.password"
                  type="password"
                  maxlength="63"
                  autocomplete="new-password"
                  :placeholder="i.ssid && i.ssid === i.savedSSID ? t('network.wifi.keepPassword') : ''"
                />
              </div>
            </div>
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
        <Button type="submit" size="sm" :disabled="busy || !!change || !!status.pending || !form.adminPassword || wifiIncomplete">
          <LoaderCircle v-if="busy" class="animate-spin" />{{ t("network.apply") }}
        </Button>
      </form>
    </template>
    <Skeleton v-if="!status && !loadError" class="h-48 rounded-xl" />
  </div>
</template>
