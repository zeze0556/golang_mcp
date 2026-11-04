# Multi-SSH MCP Server

基于 Golang + [mcp-go](https://github.com/mark3labs/mcp-go) 的多服务器 SSH 管理 MCP 服务。
支持 **stdio** 与 **Streamable HTTP** 两种传输模式，可通过 nginx 反代暴露给网页前端（如 llama.cpp Web UI）。

## 功能

| 工具 | 说明 |
|------|------|
| `list_servers` | 列出所有已配置的远程服务器 |
| `execute_command` | 在指定服务器执行 Shell 命令并回显 stdout/stderr |
| `read_file` | 读取远程文件内容 |
| `write_file` | 写入/覆盖远程文件（经 SFTP 直写，不经过 shell） |
| `read_local_file` | 读取本地文件（**默认关闭**，需 `local_read` 白名单开启） |

## 安全与健壮性说明

- **SSH 主机密钥校验（默认开启）**：除显式设置 `insecure_ignore_host_key: true` 的服务器外，所有连接均通过 `known_hosts` 校验，防止中间人攻击。首次连接前请先用 `ssh-keyscan` 把目标主机写入 `~/.ssh/known_hosts`（或全局 `known_hosts_path`）。若文件缺失或主机未知，连接会失败关闭并给出明确提示。
- **本地文件读取默认关闭**：`read_local_file` 默认不可用；需在 `local_read.enabled: true` 并配置 `allowed_dirs` 目录白名单后才放行，避免被（即便持有令牌的）调用方读取 SSH 私钥、`config.yaml` 等敏感本地文件。
- **命令执行有超时与输出上限**：`command.timeout_seconds`（默认 120s）超时即中断远端进程；`command.max_output_mb`（默认 4MB）超出自动截断，防止超大输出撑爆响应。
- **工具注解**：所有工具已标注 `readOnlyHint` / `destructiveHint` / `openWorldHint`，便于客户端对危险操作做安全门禁。
- **结构化输出**：`list_servers` 同时返回结构化内容（JSON，字段 `count` / `servers`）与文本回退，便于程序化消费。
- **操作审计**：工具的 `reason` 参数会进入结构化日志（`mcp.tool.done` / `mcp.tool.failed` 均带 `reason` 字段），可用于事后审计。

> 协议版本由客户端与服务端自动协商，无需手动修改；当前 mcp-go 基线支持到 `2025-06-18` / `2025-11-25`。

## 快速开始

```powershell
# 1. 编译（生成 ssh-mcp-server.exe）
go build -o ssh-mcp-server.exe main.go

# 2. 编辑 config.yaml 填入你的服务器与传输配置

# 3. 运行
.\ssh-mcp-server.exe
```

## 传输模式

### stdio（默认，Claude Desktop 等本地客户端）

`config.yaml` 中 `transport: "stdio"`，Claude Desktop 配置：

```json
{
  "mcpServers": {
    "ssh-manager": {
      "command": "D:/work/mynas/golang_mcp/ssh-mcp-server.exe",
      "args": []
    }
  }
}
```

### http（网页前端 / nginx 反代）

`config.yaml` 中：

```yaml
transport: "http"
http:
  host: "0.0.0.0"
  port: 8080
  path: "/mcp"
  # 通配所有域名：仅写 "*" 即可，自动放行 mcp-protocol-version 等任意自定义头
  cors_origins:
    - "*"
  cors_credentials: true
```

启动后客户端连接地址为 `http://<host>:8080/mcp`。

#### CORS 通配说明

- `cors_origins: ["*"]` → 允许任意域名跨域访问（最适合开发 / 局域网多前端场景）。
- CORS 由 **mcp-go 库内置**（`WithStreamableHTTPCORS`）处理：预检(OPTIONS)自动放行，
  `mcp-protocol-version`、`mcp-session-id` 等自定义头已在允许列表内，不会出现
  "Failed to fetch (check CORS?)" 错误；非白名单来源则不带 CORS 头（被浏览器拦截）。
- 若只打算给特定前端用，把 `"*"` 换成具体源即可（如 `"http://192.168.2.236:8082"`）。
- 若经 nginx 反代且由 nginx 统一处理 CORS，可把 `cors_origins` 留空（不启用），由反代层负责跨域。

#### 访问令牌鉴权（推荐公网 / 反代暴露时开启）

开启后，任何对 `/mcp` 的调用都必须携带合法 `access_token`，否则返回 `401`。这能防止
服务地址泄露后被他人直接调用你的远程 SSH 工具。

`config.yaml` 中：

```yaml
http:
  auth:
    enabled: true
    tokens:
      # 方式 A: 明文令牌（仅内网/临时测试，切勿提交到代码仓库）
      - "请改成一段足够长且随机的令牌"
      # 方式 B: 从环境变量读取，避免明文落盘（推荐）
      - "${MCP_ACCESS_TOKEN}"
```

客户端两种携带方式均可（二选一）：

```http
Authorization: Bearer <token>
X-Access-Token: <token>
```

> 预检请求（OPTIONS）**不受**鉴权拦截，以保证跨域握手正常完成；仅实际请求（POST/GET）需要令牌。
> 启用了 `auth` 时，跨域实际请求未带令牌会返回 401，该 401 响应也补上了 CORS 头，浏览器能正常读取。

生成强令牌（任选其一）：

```bash
openssl rand -hex 32          # 64 位十六进制随机串
# 或
python -c "import secrets; print(secrets.token_hex(32))"
```

#### 本地验证（curl）

```bash
# 1) 健康检查（无需构造协议请求，确认服务存活）
curl http://localhost:8080/health
# => {"status":"ok","transport":"http","servers":3}

# 2) 未带令牌访问（开启 auth 后应返回 401）
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"curl","version":"1.0"}}}'
# => 401 Unauthorized

# 3) 携带令牌的 MCP 握手 + 列出工具（模拟一次完整调用）
#    注意必须带 Content-Type: application/json，否则返回 400
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Authorization: Bearer 你的令牌" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"curl","version":"1.0"}}}'
# 响应头里会带 Mcp-Session-Id，后续请求需带上它
```

> 常见坑：直接 `curl http://host:port/mcp`（默认 GET）会因缺少会话 / 协议头而无数据返回，
> 必须用 `-X POST` 并带 `Content-Type: application/json`。

## Nginx 反向代理

参考 `nginx-mcp.conf`。核心要点：
- `proxy_http_version 1.1` + `proxy_buffering off`（支持 SSE 流式）
- `proxy_read_timeout` 设大（远程命令可能执行很久）
- 传递 `Mcp-Session-Id` 头（有状态模式依赖）

## 与 llama.cpp Web 前端集成

llama.cpp 的 `llama-server` 支持接入外部 MCP 服务器。在其 MCP 配置中指向本服务的 HTTP 端点：

```json
{
  "mcpServers": {
    "ssh-remote": {
      "type": "http",
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

启动 llama-server 时加载该配置（具体参数见 llama.cpp 文档 `--mcp-server` / `--mcp-config`），
其 Web 前端即可通过 llama.cpp 间接调用本服务的远程 SSH 工具。

> 若网页前端直接以浏览器 JS 调用本服务（非经 llama.cpp），需确保 `cors_origins`
> 包含前端域名，且浏览器请求携带正确的 `Mcp-Session-Id`。

## 用 fail2ban 封禁暴力破解（推荐经 nginx 反代时启用）

鉴权失败由 Go 返回 `401`，nginx 在 `combined` 日志中已记录真实客户端 IP 与状态码，
**无需改动 Go 代码**，fail2ban 直接读 nginx 日志即可封禁反复失败的 IP。

### 1. nginx 侧（已在 `nginx-mcp.conf` 配置）

- `/mcp` 位置已加 `access_log /var/log/nginx/mcp-access.log;`（专用日志，便于 fail2ban 隔离读取）。
- 若改用 nginx 主日志，注释掉该行，fail2ban 改指 `/var/log/nginx/access.log` 即可。
- 顺手修了原 `/health` 的 `proxy_pass` 误指向 `/mcp` 的 bug（现为 `/health`）。

### 2. fail2ban 侧

把本仓库 `fail2ban/` 目录内容拷到 Linux 反代主机：

```bash
sudo cp fail2ban/filter.d/mcp-auth.conf /etc/fail2ban/filter.d/
sudo cp fail2ban/jail.d/mcp-auth.conf   /etc/fail2ban/jail.d/
# 编辑 jail.d/mcp-auth.conf，确认 logpath 与你的 nginx 实际日志路径一致
sudo fail2ban-client reload
```

### 3. 验证

```bash
# 手动触发几次失败，应出现在日志中
for i in $(seq 1 6); do curl -s -o /dev/null -X POST http://你的域名/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"x","version":"1.0"}}}'; done

# 查看 fail2ban 是否识别并开始封禁
sudo fail2ban-client status mcp-auth
sudo fail2ban-client banned mcp-auth
```

### 4. 注意

- 若 nginx 前面还有 CDN / 另一层代理，需启用 nginx `real_ip` 模块，否则 `$remote_addr`
  是上一层代理 IP，会封错对象。
- `maxretry` / `findtime` / `bantime` 在 jail 配置中按需调整。
- **仅在经 nginx 暴露时适用**（fail2ban 为 Linux 工具）。若你直接把 Go 服务暴露且无 nginx，
  则需要 Go 自己按 nginx 格式写访问日志——届时告知我，我再给 Go 加一个本地 access.log 输出。
