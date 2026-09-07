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

# Stage 3: static docker CLI — Dagger's engine "image" driver shells out to
# `docker` to run the engine as a sibling container. Must be the static build
# (distroless has no libc loader for the musl-linked apk package).
FROM alpine:3.20 AS dockercli
ARG DOCKER_CLI_VERSION=27.3.1
RUN wget -qO- "https://download.docker.com/linux/static/stable/x86_64/docker-${DOCKER_CLI_VERSION}.tgz" \
    | tar xz --strip-components=1 -C /etc docker/docker

# Stage 4: minimal runtime
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/petal /petal
COPY --from=dockercli /etc/docker /usr/local/bin/docker
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/petal"]
CMD ["-db", "/data/petal.db"]
