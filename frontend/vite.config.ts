import { execSync } from "node:child_process";
import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";

function gitCommit(): string {
  try {
    return execSync("git rev-parse --short HEAD", {
      encoding: "utf-8",
    }).trim();
  } catch {
    return "unknown";
  }
}

const apiTarget = process.env.VITE_API_TARGET ?? "http://127.0.0.1:8080";

function apiTargetOrigin(target: string): string {
  try {
    return new URL(target).origin;
  } catch {
    return target;
  }
}

function isViteDevOrigin(
  origin: string | undefined,
  host: string | undefined,
): boolean {
  if (!origin || !host) return false;
  try {
    const u = new URL(origin);
    return u.protocol === "http:" && u.host === host;
  } catch {
    return false;
  }
}

export default defineConfig({
  base: "/",
  plugins: [svelte()],
  define: {
    "import.meta.env.VITE_BUILD_COMMIT": JSON.stringify(
      gitCommit(),
    ),
  },
  resolve: {
    conditions: ["browser"],
  },
  server: {
    proxy: {
      "/api": {
        target: apiTarget,
        changeOrigin: true,
        configure(proxy) {
          proxy.on("proxyReq", (proxyReq, req) => {
            if (isViteDevOrigin(req.headers.origin, req.headers.host)) {
              proxyReq.setHeader("Origin", apiTargetOrigin(apiTarget));
            }
          });
        },
      },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  test: {
    environment: "jsdom",
    exclude: ["e2e/**", "node_modules/**"],
    server: {
      deps: {
        inline: ["svelte"],
      },
    },
  },
});
