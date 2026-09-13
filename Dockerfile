# syntax=docker/dockerfile:1.7
FROM golang:1.24-alpine AS build

ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/01agent ./cmd/01agent

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/01agent /usr/local/bin/01agent
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/01agent"]
