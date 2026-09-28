import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The panel is built into ../internal/adapters/webui/dist and embedded in the
// binary with embed.FS. There is no separate static host, no CDN and no build
// step at deploy time: the server that answers the API is the same process
// that serves the page it runs in.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "../internal/adapters/webui/dist",
    emptyOutDir: true,
    // Named without hashes so the embedded file set is stable across builds
    // and a rebuild produces a reviewable diff instead of a wall of renames.
    rollupOptions: {
      output: {
        entryFileNames: "assets/panel.js",
        chunkFileNames: "assets/[name].js",
        assetFileNames: "assets/panel.[ext]",
      },
    },
  },
  server: {
    // In development the panel runs on Vite and proxies the API to a local
    // server, so the same code path (relative /api URLs) works in both.
    proxy: {
      "/api": "http://127.0.0.1:9000",
    },
  },
});
