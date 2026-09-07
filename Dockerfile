# syntax=docker/dockerfile:1

# Stage 1: build the frontend
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: build the static Go binary (pure-Go SQLite -> CGO_ENABLED=0)
# Keep this Go version in sync with the toolchain directive in go.mod.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/petal .

# Stage 3: minimal runtime
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/petal /petal
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/petal"]
CMD ["-db", "/data/petal.db"]
