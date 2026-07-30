# ============================================================
# Multi-SSH MCP Server —— 多阶段构建镜像
# ------------------------------------------------------------
# 阶段 1 (builder): 官方 Go 镜像内静态编译二进制
# 阶段 2 (runtime): 轻量 alpine 运行，镜像极小、无源码/无私钥
#
# 构建上下文 = 项目根目录（见 docker-compose.yml 的 context: ..）
# 本 Dockerfile 位于 docker/ 子目录，因此 COPY 源路径以项目根为基准
# ============================================================

# ---------------- 阶段 1: 编译 ----------------
# 注意: go.mod 声明 go >= 1.26.4，必须用不低于此版本的标签。
# 若构建报 "go.mod requires go >= 1.26.4"，把下面改为 golang:1.26.4 或更高。
FROM golang:1.26.4 AS builder

# 静态编译：不依赖宿主机 libc，alpine/distroless 都能跑
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

WORKDIR /src

# 先拷依赖清单，利用层缓存：只有 go.mod/go.sum 变动才重拉依赖
COPY go.mod go.sum ./
RUN go mod download

# 再拷源码（当前项目仅 main.go 一个源文件）
COPY main.go ./

# 编译产物（去掉调试信息、路径，进一步瘦身）
RUN go build -trimpath -ldflags="-s -w" -o /out/ssh-mcp-server .

# ---------------- 阶段 2: 运行 ----------------
FROM alpine:3.20

# 仅装 wget，供 HEALTHCHECK 探测 /health 端点（无需其他运行依赖）
RUN apk add --no-cache wget

# 创建非 root 用户，降低容器逃逸风险（服务监听 8080 非特权端口）
RUN addgroup -S mcp && adduser -S mcp -G mcp

WORKDIR /app

# 仅拷贝编译产物与启动脚本；源码、依赖、私钥绝不进入运行镜像
# 注意 entrypoint.sh 在 docker/ 子目录（构建上下文为项目根）
COPY --from=builder /out/ssh-mcp-server /app/ssh-mcp-server
COPY docker/entrypoint.sh /app/entrypoint.sh

RUN chmod +x /app/ssh-mcp-server /app/entrypoint.sh \
    && chown -R mcp:mcp /app

# 配置文件由 compose 通过 volume 挂载到 /app/config.yaml（见 docker-compose.yml）
# 程序从当前工作目录读取 config.yaml，因此 WORKDIR 必须是 /app
USER mcp

EXPOSE 8080

# 镜像自带健康检查；docker compose ps 可直接看到状态
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/app/entrypoint.sh"]
