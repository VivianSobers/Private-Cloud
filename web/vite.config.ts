import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The app is served from the same origin as the API — Caddy fronts both. That
// is not a convenience: WebAuthn binds credentials to an origin, so a UI on a
// different host could not use the passkeys this server issues.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist",
    // Fail the build rather than silently shipping a bundle that quietly grew
    // past the point where a first load on a phone is unpleasant.
    chunkSizeWarningLimit: 500,
    sourcemap: true,
  },
  server: {
    port: 5173,
    // `npm run dev` talks to a locally running API. Same-origin through the
    // proxy, so passkeys work in development exactly as they do in production.
    proxy: {
      "/api": { target: "http://localhost:8080", changeOrigin: false },
    },
  },
});
