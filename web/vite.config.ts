import { resolve } from "node:path";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// In development the app is served by vite and the API by a running
// icegator, so requests under /api are proxied to it. Set ICEGATOR_API if
// the dashboard is not listening on the default port.
const API = process.env.ICEGATOR_API ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react()],
  resolve: { alias: { "@": resolve(import.meta.dirname, "src") } },
  server: { port: 7710, strictPort: true, proxy: { "/api": API } },
  build: { outDir: "dist", emptyOutDir: false },
});
