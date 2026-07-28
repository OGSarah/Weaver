# Multi-stage build. Three stages: bundle the frontend with Node, compile the static
# Go binaries, then ship both on a tiny base. ARG BINARY selects which cmd to build
# so one Dockerfile covers all three services.

# Stage 1: the frontend bundle.
#
# This stage is what lets web/dist stay out of version control. Without it a fresh
# clone would need a Node toolchain on the host before the UI would render, so the
# built bundle had to be committed instead; here the image builds it, and the only
# thing in the repo is source.
#
# Only the api serves the UI, but the runtime stage below is shared by all three
# binaries, so the worker and scheduler images carry the same few hundred KB of
# unused assets. That is the price of one Dockerfile rather than two, and BuildKit
# builds this stage once and reuses it across the three services.
FROM node:22-alpine AS web

WORKDIR /src/web

# Manifest first, so the dependency install is cached until the manifest changes
# rather than on every edit to a component. npm ci rather than npm install: it
# installs exactly what package-lock.json pins, and fails if the two disagree
# instead of quietly rewriting the lockfile.
COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
# The header wordmark is imported from docs/branding rather than duplicated inside
# web/, so it has to be in this stage too or the bundle step cannot resolve it.
COPY docs/branding/ /src/docs/branding/

RUN npm run build

# Stage 2: the Go binary.
FROM golang:1.25 AS build

WORKDIR /src

# Copy module files first so `go mod download` is cached until they change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BINARY=worker
# CGO off yields a static binary that runs on the scratch-like base below.
RUN CGO_ENABLED=0 go build -o /out/app ./cmd/${BINARY}

# Stage 3: the runtime image.
FROM gcr.io/distroless/static-debian12

COPY --from=build /out/app /app

# Only what the Go file server actually serves, not the whole web/ tree: node_modules
# is tens of megabytes and has no business in a runtime image.
COPY --from=web /src/web/index.html /web/index.html
COPY --from=web /src/web/dist /web/dist

ENTRYPOINT ["/app"]
