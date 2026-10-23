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
    proxy: { "/api": process.env.SAPO_API ?? "http://127.0.0.1:5000" },
  },
  build: { outDir: "dist", emptyOutDir: true },
})
