import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  build: {
    // Build straight into the Go embed directory. One canonical location for
    // the built client means the Dockerfile does not have to copy it around,
    // and `go build -tags embedweb` picks it up with no extra step.
    outDir: "../internal/webui/dist",
    emptyOutDir: true,
    sourcemap: true,
  },
  server: {
    port: 5173,
    // In development the client runs on Vite's dev server and the Go server
    // runs separately on :8080. Proxying keeps requests same-origin so the
    // session cookie behaves exactly as it will in production.
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: false,
      },
    },
  },
});
