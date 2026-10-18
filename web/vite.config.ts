import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const server = process.env.HELM_SERVER ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react()],
  server: { proxy: { "/api": { target: server, changeOrigin: true } } },
});
