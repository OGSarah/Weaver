# Multi-stage build: compile the static Go binaries, then ship them on a tiny
# base. ARG BINARY selects which cmd to build so one Dockerfile covers all three.
FROM golang:1.25 AS build

WORKDIR /src

# Copy module files first so `go mod download` is cached until they change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BINARY=worker
# CGO off yields a static binary that runs on the scratch-like base below.
RUN CGO_ENABLED=0 go build -o /out/app ./cmd/${BINARY}

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
