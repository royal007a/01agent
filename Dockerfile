# 环境锁定第 4 步：把 Harness 连同运行时、依赖、工具一起打包，换机器、换人跑起来都是同一个环境。
# 基础镜像按 digest 锁定（tag 会漂移，digest 不会）；升级基础镜像 = 改 digest = 一次显式的 review。
#
#   docker build -t go-tiny-claw:$(git rev-parse --short HEAD) .
#   docker run --rm -p 48080:48080 -v claw-ws:/workspace -e CLAW_HTTP_TOKEN=... -e CLAW_API_KEY=... -e CLAW_MODEL=... go-tiny-claw:<tag>
#   （容器内监听 0.0.0.0，服务端要求 CLAW_HTTP_TOKEN 才肯启动）

# BIN_FROM=source：容器内编译（默认）；BIN_FROM=prebuilt：用 make docker-prebuilt 在宿主机以同一锁定工具链
# 交叉编译好的 dist/claw-linux（构建机内存不足时用；-trimpath + CGO_ENABLED=0 + 同版本工具链，产物可复现）
ARG BIN_FROM=source

# golang:1.26 (linux/arm64+amd64 manifest list)
FROM golang@sha256:3c3e25a4da13fd0478eed2df1eb35a0e667094a7124d3993a6a1d30f71c17e79 AS toolchain
WORKDIR /src
# 与 go.mod 的 toolchain 行一致；镜像自带版本不同时从 GOPROXY 下载精确版本
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOTOOLCHAIN=go1.26.4 GOPROXY=${GOPROXY} GOFLAGS=-mod=readonly CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download && go mod verify && mkdir -p /out && cp -r "$(go env GOROOT)" /out/go

# 单阶段条件分支（兼容无 BuildKit 的旧构建器：它会构建所有阶段，不能靠多阶段选择）
FROM toolchain AS bin
ARG BIN_FROM
COPY . .
RUN if [ "$BIN_FROM" = prebuilt ]; then cp dist/claw-linux /out/claw; else go build -trimpath -o /out/claw ./cmd/claw; fi

# alpine:3.22
FROM alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
# Agent 的 bash / 检索 / 评测会调用的外部命令（与 envlock.DefaultTools 对应）
RUN apk add --no-cache bash git ripgrep grep sed ca-certificates tzdata
COPY --from=toolchain /out/go /usr/local/go
COPY --from=bin /out/claw /usr/local/bin/claw
COPY deploy/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENV PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin LC_ALL=C.UTF-8 CLAW_HOME=/state GOTOOLCHAIN=local
# 构建期留一份镜像环境快照，便于分发后比对"这台机器跑的是不是同一个镜像环境"
RUN mkdir -p /opt/claw && claw env lock -dir /opt/claw >/dev/null
VOLUME ["/workspace", "/state"]
EXPOSE 48080
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["serve", "-addr", "0.0.0.0:48080", "-workdir", "/workspace"]
