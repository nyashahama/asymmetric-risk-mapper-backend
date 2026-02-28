# ── Stage 1: Build ────────────────────────────────────────────────────────────
# Use the full Go image to compile. The binary is statically linked so the
# final image needs nothing except the binary itself.
FROM golang:1.23-alpine AS builder

# Install git (needed for `go mod download` with private modules) and
# ca-certificates (needed to make HTTPS calls at build time, e.g. go get).
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app

# Copy dependency manifests first. Docker caches this layer separately so that
# `go mod download` is only re-run when go.mod / go.sum actually change — not
# on every source file edit.
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source tree and build the binary.
# CGO_ENABLED=0  → fully static binary, no libc dependency
# GOOS/GOARCH    → explicit target (matches the scratch image below)
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
    -trimpath \
    -o /app/server \
    ./cmd/api

# ── Stage 2: Run ──────────────────────────────────────────────────────────────
# scratch is an empty image — no shell, no package manager, no attack surface.
# The binary is the only thing inside the container.
FROM scratch

# Copy the root CA bundle so outbound HTTPS calls (Stripe, Anthropic, Resend,
# DeepSeek) work correctly inside the container.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy timezone data so time.LoadLocation() works if you ever need it.
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy the compiled binary.
COPY --from=builder /app/server /server

# Document the port the app listens on. The actual binding is controlled by
# the PORT env var read in config.Load(); this is informational for Docker /
# container orchestrators.
EXPOSE 8080

# Run as a non-root user (numeric UID avoids needing /etc/passwd in scratch).
USER 65534:65534

ENTRYPOINT ["/server"]