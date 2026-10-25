# syntax=docker/dockerfile:1

# Frontend stage. The Vite artifact (cmd/niceat/web/index.html) is a build
# product, deliberately not committed, so the image builds it from source
# here; the Go stage below embeds the result and needs no Node toolchain.
FROM node:24-alpine AS frontend
WORKDIR /src/frontend
RUN npm install -g pnpm@12
COPY frontend/package.json frontend/pnpm-lock.yaml ./
# --frozen-lockfile: the lockfile is the spec; a deploy build must never
# quietly resolve newer versions than the ones the frontend was tested with.
RUN pnpm install --frozen-lockfile
COPY frontend ./
# vite's outDir is ../cmd/niceat/web (vite.config.ts), so the artifact lands
# exactly where the Go stage copies it from.
RUN pnpm build

# Build stage.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY --from=frontend /src/cmd/niceat/web/index.html ./cmd/niceat/web/
# CGO_ENABLED=0: the distroless runtime stage ships no libc, so the binary
# must be static; -trimpath and -s -w shave it to a few MB.
# /app is pre-created and chowned to nonroot (65532) so the nonroot runtime
# owns its working directory (the -config default is injected there); the
# reaper keeps no local state, its grace clocks live in the DNS zones.
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/niceat ./cmd/niceat \
    && mkdir /out/app \
    && chown 65532:65532 /out/app

# Distroless static ships the CA certificates the Porkbun client and the
# reaper's DID resolver need for TLS, plus a nonroot user, and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
# Copy the (empty) app directory first so /app itself lands owned by nonroot;
# copying only the binary would create a root-owned /app.
COPY --from=build --chown=65532:65532 /out/app /app
COPY --from=build --chown=65532:65532 /out/niceat /app/niceat
WORKDIR /app
ENTRYPOINT ["/app/niceat"]
