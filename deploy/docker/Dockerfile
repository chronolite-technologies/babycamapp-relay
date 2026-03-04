# ── Build stage ─────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /babycam-relay .

# ── Production stage ───────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /babycam-relay /babycam-relay

ENV RELAY_PORT=8080
ENV RELAY_TTL=120
ENV RELAY_MAX_BODY=16384
ENV RELAY_RATE_LIMIT=10
ENV RELAY_MAX_ROOMS=128

EXPOSE 8080

ENTRYPOINT ["/babycam-relay"]
