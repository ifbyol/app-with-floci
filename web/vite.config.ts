import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The dev server proxies /api so the browser sees a single origin. API_PROXY
// points at localhost during `npm run dev` on a laptop and at the api service
// when running inside a cluster under `okteto up`.
const target = process.env.API_PROXY ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: { "/api": { target, changeOrigin: true } },
  },
  build: { outDir: "dist", sourcemap: false },
});
