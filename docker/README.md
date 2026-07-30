# Docker 部署 Multi-SSH MCP Server

把编译与运行完整容器化。镜像采用**多阶段构建**：Go 镜像内静态编译二进制，再用 alpine 运行，
最终镜像不含源码、不含私钥、不含 `go` 工具链，体积小且攻击面小。

## 目录结构

```
docker/
├── Dockerfile          # 多阶段构建（builder: golang → runtime: alpine）
├── docker-compose.yml  # 编排：构建 + 端口映射 + 配置挂载 + 健康检查
├── entrypoint.sh       # 容器入口：配置缺失时友好报错，再前台启动服务
├── .env.example        # 访问令牌模板（MCP_ACCESS_TOKEN）
└── README.md           # 本文件
.dockerignore           # 位于项目根（构建上下文根），排除敏感/无关文件
```

> `.dockerignore` 必须放在**项目根目录**而非 `docker/`：因为 `docker-compose.yml`
> 的构建上下文是 `..`（项目根），Docker 只读取上下文根目录的 `.dockerignore`。
> 它负责把 `config.yaml`、`openclaw`(私钥)、`*.key`、`.env` 等敏感文件挡在 build context 之外。

## 快速开始

```bash
cd docker

# 1) （可选）准备访问令牌。留空则需在 config.yaml 写明文令牌
cp .env.example .env
#   编辑 .env，填入 MCP_ACCESS_TOKEN=（openssl rand -hex 32 生成）
#   注：compose 会自动加载同目录的 .env 作为变量源，无需在 compose 里再引用

# 2) 准备好项目根的 config.yaml（参考根目录 config.example.yaml）
#    容器化部署关键：servers 里用 key_content 而非 key_path

# 3) 构建并后台启动
docker compose up -d --build

# 4) 查看状态与日志
docker compose ps          # STATUS 应为 healthy
docker compose logs -f mcp
```

停止 / 重建：

```bash
docker compose down                 # 停止并移除容器
docker compose up -d --build        # 代码改动后重新构建
```

## 配置要点（容器内必读）

### 1. 传输模式

容器里通常用 **http**（`config.yaml` 中 `transport: "http"`，默认监听 `0.0.0.0:8080`）。
`stdio` 模式适合本地子进程（如 Claude Desktop 直接拉起 exe），在容器里没有意义。

如需临时切换，可不改配置文件，直接设环境变量：

```bash
MCP_TRANSPORT=http docker compose up -d   # 入口脚本会把该变量透传给 Go 程序
```

### 2. 私钥：用 key_content，不要用 key_path

`key_path` 在 `config.yaml` 里通常是 Windows 路径（如 `C:/Users/.../.ssh/id_ed25519`），
容器内不存在该路径，会导致连接失败。容器化部署请把私钥内容直接贴进 `key_content`：

```yaml
servers:
  web-1:
    host: "192.168.1.21"
    port: 22
    user: "root"
    key_content: |
      -----BEGIN OPENSSH PRIVATE KEY-----
      （把私钥全文粘贴到这里，注意缩进与换行）
      -----END OPENSSH PRIVATE KEY-----
    description: "前端应用服务器"
```

### 3. 访问令牌：走环境变量，不落盘

`config.yaml` 中已支持 `${MCP_ACCESS_TOKEN}` 占位：

```yaml
http:
  auth:
    enabled: true
    tokens:
      - "${MCP_ACCESS_TOKEN}"
```

把真实令牌放进 `docker/.env`（compose 自动加载为变量源，注入到容器的 `MCP_ACCESS_TOKEN` 环境变量），
**不要**把明文令牌写进 `config.yaml`，否则会与仓库/镜像管理边界混淆。

### 4. 端口一致性

`docker-compose.yml` 映射 `8080:8080`。若你修改了 `config.yaml` 的 `http.port`，
必须同步改 compose 的 `ports`（否则外部访问不到）。

## 本地验证

```bash
# 健康检查（无需构造协议请求）
curl http://localhost:8080/health
# => {"status":"ok","transport":"http","servers":N}

# 携带令牌的 MCP 初始化握手（把 <token> 换成 MCP_ACCESS_TOKEN 的值）
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <token>" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"curl","version":"1.0"}}}'
```

## 与 nginx / fail2ban 配合

镜像只提供 MCP 服务本身。若需公网暴露并防暴力破解，仍按根目录 `README.md` 的方案：
在本机另起 nginx 反代（参考 `nginx-mcp.conf`）+ fail2ban（参考 `fail2ban/`）。
容器内服务监听 `0.0.0.0:8080`，nginx 用 `proxy_pass http://127.0.0.1:8080;` 指向本容器映射出来的宿主机端口即可。

## 常见问题

- **`config.yaml: no such file` / 入口脚本报未找到配置**
  宿主机的 `config.yaml` 不存在，或路径不对。compose 挂载的是 `../config.yaml`（项目根），
  请确认项目根有该文件（被 `.gitignore` 忽略，不会进仓库，需自行准备）。

- **SSH 连接报读取私钥失败**
  八成是用了 `key_path` 的 Windows 路径。改 `key_content` 贴私钥全文。

- **构建报 `go.mod requires go >= 1.26.4`**
  `Dockerfile` 的 builder 基础镜像版本过低。把 `FROM golang:1.26.4` 提升到更高的 1.26.x 标签。

- **`docker compose ps` 一直 starting / unhealthy**
  多半是配置里 `servers` 为空或 `transport` 写错。看 `docker compose logs mcp` 定位。
