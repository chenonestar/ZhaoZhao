import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// 构建产物输出到 Go 的内嵌目录；开发时把 /api 代理到本机运行的后端。
export default defineConfig({
  plugins: [react()],
  build: { outDir: "../internal/web/dist", emptyOutDir: true },
  server: { proxy: { "/api": process.env.ZZ_BACKEND || "http://127.0.0.1:17890" } },
});
