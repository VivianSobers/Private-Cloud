# Build-only image: it produces the SPA bundle and nothing else.
#
# There is no Node process in production. The compose `web` service runs this
# once, copies /dist into a volume, and exits; Caddy serves the static files.
# Shipping a Node runtime just to serve pre-built assets would add an attack
# surface and a memory footprint for no benefit.

FROM node:22-alpine AS build

WORKDIR /app

# Dependencies first, as their own layer: package.json changes far less often
# than source, so a normal edit-rebuild cycle skips the install entirely.
COPY package.json package-lock.json* ./
# `npm ci` when a lockfile is present — it installs exactly what was locked and
# fails loudly on drift, which is the whole point of committing one. Falls back
# to `npm install` so a fresh clone without a lockfile still builds.
RUN if [ -f package-lock.json ]; then npm ci --no-audit --no-fund; \
    else npm install --no-audit --no-fund; fi

COPY tsconfig.json vite.config.ts index.html ./
COPY src ./src

# `npm run build` runs tsc --noEmit before vite, so a type error fails the
# image build rather than shipping a bundle that misuses the API.
RUN npm run build

# The final stage carries only the built assets. busybox rather than scratch
# because the compose command needs a shell to copy them into the volume.
FROM busybox:1.37-musl

COPY --from=build /app/dist /dist
