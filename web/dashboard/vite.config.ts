import path from "path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// https://vite.dev/config/
export default defineConfig({
  base: "/",
  plugins: [react(), tailwindcss()],
  build: {
    // Build directly into the Go //go:embed root (internal/web/dist) so the
    // single binary picks up the freshly built assets with no copy step.
    outDir: path.resolve(__dirname, "../../internal/web/dist"),
    assetsDir: "dashboard-assets",
    emptyOutDir: true,
  },
  server: {
    host: "0.0.0.0",
    port: 5173,
    proxy: {
      "/__debug": {
        target: "http://127.0.0.1:3000",
        changeOrigin: true,
      },
    },
  },
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
})
