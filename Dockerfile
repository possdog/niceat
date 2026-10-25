# syntax=docker/dockerfile:1

# Build stage. The frontend artifact (cmd/niceat/web/index.html) is committed,
# so a plain go build suffices — no Node toolchain in the image chain.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
# CGO_ENABLED=0: the distroless runtime stage ships no libc, so the binary
# must be static; -trimpath and -s -w shave it to a few MB.
# /app is pre-created and chowned to nonroot (65532) because the runtime works
# there (the -config default and the reaper's reaper_state.json both live in
# the working directory) and the distroless nonroot user must be able to
# write the state file.
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/niceat ./cmd/niceat \
    && mkdir /out/app \
    && chown 65532:65532 /out/app

# Distroless static ships the CA certificates the Porkbun client and the
# reaper's DID resolver need for TLS, plus a nonroot user, and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
# Copy the (empty) app directory first so /app itself lands owned by nonroot;
# copying only the binary would create a root-owned /app the reaper cannot
# write its state file into.
COPY --from=build --chown=65532:65532 /out/app /app
COPY --from=build --chown=65532:65532 /out/niceat /app/niceat
WORKDIR /app
ENTRYPOINT ["/app/niceat"]
