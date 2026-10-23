import { fileURLToPath, URL } from "node:url"
import tailwindcss from "@tailwindcss/vite"
import vue from "@vitejs/plugin-vue"
import { defineConfig } from "vite"

// Panel dibangun ke web/dist lalu disajikan oleh sapo-api.
// Saat pengembangan, /api diteruskan ke sapo-api lokal.
export default defineConfig({
  plugins: [vue(), tailwindcss()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  server: {
    // Host asli (localhost:5173) dipertahankan: sapo-api memeriksa bahwa Origin
    // sama dengan Host (CSRF), dan bentuk singkat proxy Vite menggantinya.
    proxy: { "/api": { target: process.env.SAPO_API ?? "http://127.0.0.1:5000", changeOrigin: false } },
  },
  build: { outDir: "dist", emptyOutDir: true },
})
