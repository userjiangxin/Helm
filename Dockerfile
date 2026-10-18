# helm-server + built board in one image. Daemons (helmd) run on the machines themselves, not here.
FROM node:22-bookworm-slim AS web
RUN corepack enable
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM node:22-bookworm-slim
RUN corepack enable
WORKDIR /app/server
COPY server/package.json server/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile --prod=false
COPY server/ ./
COPY db/ /app/db/
COPY --from=web /src/web/dist /app/web/dist
ENV HOST=0.0.0.0 PORT=8080
EXPOSE 8080
CMD ["pnpm", "start"]
