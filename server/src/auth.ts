// Board access control (daemons authenticate separately with machine tokens).
//
//   HELM_ALLOWED_USERS=you@example.com   trust the identity header `tailscale serve` adds
//                                        (Tailscale-User-Login) and admit only these logins
//   HELM_BOARD_PASSWORD=…                otherwise / additionally: HTTP Basic auth (user "helm")
//   neither                              open — only acceptable when bound to 127.0.0.1 for local use
import type { MiddlewareHandler } from "hono";
import { timingSafeEqual } from "node:crypto";

export interface AuthConfig {
  allowedUsers: string[];
  password?: string;
}

export function authFromEnv(env = process.env): AuthConfig {
  return {
    allowedUsers: (env.HELM_ALLOWED_USERS ?? "").split(",").map((s) => s.trim().toLowerCase()).filter(Boolean),
    password: env.HELM_BOARD_PASSWORD || undefined,
  };
}

const eq = (a: string, b: string) => {
  const x = Buffer.from(a), y = Buffer.from(b);
  return x.length === y.length && timingSafeEqual(x, y);
};

export function boardAuth(cfg: AuthConfig): MiddlewareHandler {
  const open = cfg.allowedUsers.length === 0 && !cfg.password;
  return async (c, next) => {
    if (open) return next();
    const login = c.req.header("Tailscale-User-Login")?.toLowerCase();
    if (login && cfg.allowedUsers.includes(login)) return next();
    if (cfg.password) {
      const h = c.req.header("Authorization") ?? "";
      if (h.startsWith("Basic ")) {
        const [, pass = ""] = Buffer.from(h.slice(6), "base64").toString().split(/:(.*)/s);
        if (eq(pass, cfg.password)) return next();
      }
      c.header("WWW-Authenticate", 'Basic realm="Helm", charset="UTF-8"');
      return c.text("需要登录", 401);
    }
    return c.text(login ? `账号 ${login} 无权访问这个 Helm` : "请通过 Tailscale 访问 Helm", 403);
  };
}
