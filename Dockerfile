# Dockerfile for the LLM Relay gateway (Go).
#
# Multi-stage build:
#   1. Build the React console frontend (Vite) from web/dashboard/.
#   2. Compile the Go binary with the built frontend embedded.
#   3. Ship a minimal distroless image.
#
# Build:
#   docker build -t llm-relay .
# Run (via docker-compose):
#   docker compose up -d

# ---------- Stage 1: build the frontend ----------
FROM node:20-alpine AS frontend-build
WORKDIR /app/web/dashboard
# Copy the whole project so node_modules / package.json / vite.config all
# come together. (The dashboard source lives in web/dashboard/.)
COPY web/dashboard/ ./
RUN npm install --no-audit --no-fund && npm run build
# Vite outputs to ../../dist/frontend relative to web/dashboard/, i.e.
# /app/dist/frontend — we then copy that into the Go embed path.

# ---------- Stage 2: build the Go binary ----------
FROM golang:1.26-alpine AS go-build
WORKDIR /src

# Cache deps first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Overlay the built frontend into the Go embed path.
COPY --from=frontend-build /app/dist/frontend ./internal/web/dist

# Static, stripped binary for a small image.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/llm-relay ./cmd/server

# ---------- Stage 3: minimal runtime image ----------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=go-build /out/llm-relay /llm-relay

# Default port; override with -e PORT.
EXPOSE 3300
ENTRYPOINT ["/llm-relay"]
