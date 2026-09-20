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
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w" \
    -o /out/01agent-feishu ./cmd/01agent-feishu
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/01agent-eval ./cmd/01agent-eval
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/01agent-replay ./cmd/01agent-replay
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w" \
    -o /out/01agent-sandbox ./cmd/01agent-sandbox

FROM alpine:3.22
RUN apk add --no-cache bash ca-certificates
COPY --from=build /out/01agent /usr/local/bin/01agent
COPY --from=build /out/01agentd /usr/local/bin/01agentd
COPY --from=build /out/01agent-feishu /usr/local/bin/01agent-feishu
COPY --from=build /out/01agent-eval /usr/local/bin/01agent-eval
COPY --from=build /out/01agent-replay /usr/local/bin/01agent-replay
COPY --from=build /out/01agent-sandbox /usr/local/bin/01agent-sandbox
COPY evals /workspace/evals
COPY README.md /workspace/README.md
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/01agent"]
