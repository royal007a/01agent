# syntax=docker/dockerfile:1.7
FROM golang:1.25-alpine AS build

ARG VERSION=dev
ARG GOPROXY=https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN GOPROXY="${GOPROXY}" go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/01agent ./cmd/01agent
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/01agentd ./cmd/01agentd

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/01agent /usr/local/bin/01agent
COPY --from=build /out/01agentd /usr/local/bin/01agentd
COPY README.md /workspace/README.md
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/01agent"]
