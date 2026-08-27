# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# ---- dev (used by `okteto up`: keeps the toolchain for hot rebuilds) ----
FROM golang:1.26-alpine AS dev
WORKDIR /app
RUN apk add --no-cache git curl
COPY go.mod go.sum ./
RUN go mod download
EXPOSE 8080
CMD ["go", "run", "./cmd/api"]

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/api /api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/api"]
