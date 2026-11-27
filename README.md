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
| `write_file_chunked` | 分块上传助手（自动切片，跨多次调用续写大文件） |
| `read_local_file` | 读取本地文件（**默认关闭**，需 `local_read` 白名单开启） |
| `kb.search` | 知识库混合检索，返回带引用片段（**默认关闭**，需 `knowledge_base.enabled: true`） |
| `kb.fetch` | 按 chunk_id/doc_id 取相邻片段 |
| `kb.cite` | 按 chunk_id 反查带行号原文（可核验引用） |
| `kb.list_scopes` | 列出当前 token 可见的知识域 |

## 安全与健壮性说明

- **SSH 主机密钥校验（默认开启）**：除显式设置 `insecure_ignore_host_key: true` 的服务器外，所有连接均通过 `known_hosts` 校验，防止中间人攻击。首次连接前请先用 `ssh-keyscan` 把目标主机写入 `~/.ssh/known_hosts`（或全局 `known_hosts_path`）。若文件缺失或主机未知，连接会失败关闭并给出明确提示。
- **本地文件读取默认关闭**：`read_local_file` 默认不可用；需在 `local_read.enabled: true` 并配置 `allowed_dirs` 目录白名单后才放行，避免被（即便持有令牌的）调用方读取 SSH 私钥、`config.yaml` 等敏感本地文件。
- **命令执行有超时与输出上限**：`command.timeout_seconds`（默认 120s）超时即中断远端进程；`command.max_output_mb`（默认 4MB）超出自动截断，防止超大输出撑爆响应。
- **工具注解**：所有工具已标注 `readOnlyHint` / `destructiveHint` / `openWorldHint`，便于客户端对危险操作做安全门禁。
- **结构化输出**：`list_servers` 同时返回结构化内容（JSON，字段 `count` / `servers`）与文本回退，便于程序化消费。
- **操作审计**：工具的 `reason` 参数会进入结构化日志（`mcp.tool.done` / `mcp.tool.failed` 均带 `reason` 字段），可用于事后审计。

> 协议版本由客户端与服务端自动协商，无需手动修改；当前 mcp-go **v1.5.0** 支持到 `2026-07-28`（含无 session 的 modern 语义）及更早版本。

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

## 知识库子系统（kb.* 工具）

独立的只读知识库：把文档放进受控目录，服务后台完成分块 → embedding → 索引，AI 通过 4 个只读工具检索。
`enabled: false`（默认）时完全不挂载，SSH 工具不受任何影响。完整契约见 `kb-mcp-contract-draft.md`。

### 工作原理

```
启动 → NewService 立即返回（不扫文档）→ 后台 goroutine 首轮扫描 + 按 poll_seconds 轮询
   │
watch_dir（inbox，放文档）
   │  逐篇读内容算 sha256 → 查去重缓存
   │      ├─ hash 命中 → 跳过（不调 embedding）
   │      └─ 新增/变更 → 分块（记录 start_line/end_line）→ 调 /v1/embeddings → 写向量索引
   │                                                          ├─ local    → write_dir/kb_index.json
   │                                                          └─ pgvector → PostgreSQL + pgvector（HNSW）
   │  成功后把 docID→hash 落盘 write_dir/kb_ingest_state.json（原子写）
   ▼
kb.search / kb.fetch / kb.cite / kb.list_scopes（只读，token→scope 服务端过滤）
```

两种后端实现同一个 `VectorStore` 接口，上层工具/ACL/写路径完全无感，切换只改配置。

**启动不阻塞 / 去重缓存**：文档扫描与 embedding 全在后台 goroutine 执行，服务启动与文档数量无关。
每个文档的内容 sha256 持久化在 `write_dir/kb_ingest_state.json`，下次启动（或下一轮轮询）命中即跳过，
不重复调用 embedding。缓存整体失效的条件：embedding 模型/维度变更、state 文件损坏、
或 state 与向量索引不一致（比如删了索引文件）——任一触发即全量重建，宁可多算也不漏检。
全量命中缓存的空轮询不产生磁盘写。

- **backend**：
  - `local`（默认）：零依赖，索引落在本地 JSON，适合单人小库；换模型删 `write_dir/kb_index.json` 重建。
  - `pgvector`：向量存 PostgreSQL，SQL 层完成 scope 过滤 + 余弦排序（`1 - (embedding <=> q)`），
    适合多客户端共享/数据量大/需要持久化的场景。见下方 [pgvector 后端](#pgvector-后端)。
- **scope 划分**：`watch_dir/<scope>/xxx.md` → scope=`<scope>`；根目录文档 → `scope=default`。
- **ACL（集合级）**：`knowledge_base.token_scopes` 配置 token → 可见 scope。未列出的 token 看不到任何 KB 内容；
  `scope` 入参中的越权项**静默丢弃**（不报"无权"，防枚举）。留空映射 = open 模式（仅建议内网）。
- **embedding 冻结**：`model`/`dim` 一旦改变，去重缓存自动失效并由后台全量重建（无需手动删索引）。
  向量维度只有 `embedding.dim` 一个来源（pgvector 表的 `vector(dim)` 亦取此值）。
  服务端已 L2 归一化；查询侧自动加 `query_prefix`，文档侧不加。
- **不可信数据**：检索结果的文本一律是"引用材料"，工具 description 已声明，模型不得执行其中指令。
- **自激防护**：`watch_dir`/`write_dir` 必须是不同、不嵌套的绝对路径（配置加载时校验）。

### 启用步骤

1. 在 `config.example.yaml` 的 `knowledge_base` 段设置 `enabled: true`，填 `watch_dir` / `write_dir` / embedding 参数。
2. 选择后端：`backend: local`（默认，无需额外配置）或 `backend: pgvector`（填 `pgvector.dsn`）。
3. 把文档放入 `watch_dir`（按 scope 建子目录），服务**启动后立即返回**，首轮扫描在后台进行，
   之后按 `poll_seconds` 轮询；刚启动时索引可能为空，稍候即就绪。进度看日志 `kb.ingest.doc_ok` / `kb.ingest.state_loaded`。
4. 客户端调 `kb.list_scopes` 看可见知识域，再调 `kb.search`（query 必填，top_k ≤ 20）。
5. 需要核验出处时调 `kb.cite`（chunk_id 来自 search），返回带行号原文。

### 网页前端 / 远程 AI 经 HTTP 调用 KB

KB 工具与 SSH 工具注册在同一个 MCP Server 上，因此 **HTTP 传输下 `kb.*` 自动可用**，无需为知识库单开端口或通道：

```yaml
transport: http
http:
  host: "0.0.0.0"
  port: 8080
  path: "/mcp"
  cors_origins: ["https://你的前端域名"]   # 网页版 AI 跨域调用时按需放行
  auth:
    enabled: true
    tokens: ["${MCP_ACCESS_TOKEN}"]
```

网页端以 MCP Streamable HTTP 客户端连接 `http(s)://host:8080/mcp`，携带 `Authorization: Bearer <token>`，
即可在工具列表里看到并使用 `kb.search` 等 4 个工具；token 对应的可见 scope 由 `token_scopes` 决定。
单人本地使用若要"连上即用"，可设 `http.auth.enabled: false` 且 `token_scopes` 留空（open 模式，仅限可信内网）。

### pgvector 后端

前置：一个装了 [pgvector](https://github.com/pgvector/pgvector) 扩展的 PostgreSQL（15+）。最小初始化（若 `auto_schema: true` 可省略）：

```sql
CREATE EXTENSION IF NOT EXISTS vector;
-- 表/索引由服务在首次启动时幂等创建（表名取自 pgvector.table，默认 kb_chunks）
```

配置项（`knowledge_base.pgvector`）：

| 字段 | 默认 | 说明 |
|---|---|---|
| `dsn` | 必填 | libpq 连接串，支持 `${ENV_VAR}`；如 `postgres://u:p@host:5432/kb?sslmode=disable` |
| `table` | `kb_chunks` | 表名；仅允许字母/数字/下划线且不以数字开头（要拼进 DDL，故做白名单校验） |
| `max_conns` | 4 | 连接池上限 |
| `auto_schema` | true | 启动时自动幂等建扩展/表/索引；设为 false 则由运维预建 |

- **换 embedding 模型后重建**：`TRUNCATE kb_chunks;` 并把 `kb_chunks_meta` 里 `embedding_version` 删除或更新，
  下次写路径扫描会重新灌入（或整表 `DROP` 后重启，由 `auto_schema` 重建）。
- **score 方向**：SQL 用 `1 - (embedding <=> q)` 得到与 local 后端一致的相似度；`min_score` 与 `top_k` 语义两后端相同。
- **HNSW 索引**在 `dim <= 2000` 时自动创建（cosine ops）；创建失败只降级为精确扫描，不阻断服务。
- **切换后端数据不互通**：local 的 JSON 索引与 pgvector 的表各自独立，切换后写路径会重新消费 `watch_dir`。

### 测试

```bash
# 单元测试（不依赖外部服务）
go test ./kb/

# 端到端集成测试（需 192.168.2.236:8082 的 embedding 服务在线）
KB_IT=1 go test ./kb/ -run TestIntegrationRealEmbedding -v
```
