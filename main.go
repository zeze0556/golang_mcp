package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

// ==================== 模型定义 ====================

// SSHConfig 单个服务器的连接配置
type SSHConfig struct {
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	User         string `yaml:"user"`
	KeyPath      string `yaml:"key_path,omitempty"`
	KeyContent   string `yaml:"key_content,omitempty"`
	Description  string `yaml:"description,omitempty"` // 描述信息，方便 AI 理解用途
}

// AuthConfig 访问令牌鉴权配置（防止 MCP 端点被未授权访问）
type AuthConfig struct {
	Enabled bool     `yaml:"enabled,omitempty"` // 是否启用鉴权，默认 false
	Tokens  []string `yaml:"tokens,omitempty"`  // 合法的 access_token 列表（支持 ${ENV_VAR} 从环境变量读取）
}

// HTTPConfig HTTP 传输配置
type HTTPConfig struct {
	Host            string   `yaml:"host,omitempty"`             // 监听地址，默认 0.0.0.0
	Port            int      `yaml:"port,omitempty"`             // 监听端口，默认 8080
	Path            string   `yaml:"path,omitempty"`             // MCP 端点路径，默认 /mcp
	CORSOrigins     []string `yaml:"cors_origins,omitempty"`     // 允许跨域的源（网页前端）
	CORSCredentials bool     `yaml:"cors_credentials,omitempty"` // 是否允许携带凭证
	Auth            AuthConfig `yaml:"auth,omitempty"`           // 访问令牌鉴权
}

// Config YAML 配置文件结构
type Config struct {
	Transport string            `yaml:"transport,omitempty"` // stdio 或 http，默认 stdio
	HTTP      HTTPConfig        `yaml:"http,omitempty"`      // HTTP 模式配置
	Servers   map[string]SSHConfig `yaml:"servers"`
}

// SSHClient 封装 SSH 连接
type SSHClient struct {
	config *ssh.ClientConfig
	addr   string
}

// ServerManager 管理多个 SSH 连接
type ServerManager struct {
	configs map[string]SSHConfig
	clients map[string]*SSHClient
}

// ==================== SSH 客户端逻辑 ====================

// NewServerManager 从配置创建服务器管理器
func NewServerManager(cfg Config) *ServerManager {
	return &ServerManager{
		configs: cfg.Servers,
		clients: make(map[string]*SSHClient),
	}
}

// GetClient 获取或创建 SSH 连接（单例模式）
func (sm *ServerManager) GetClient(name string) (*SSHClient, error) {
	// 如果已有缓存的连接，直接返回
	if client, ok := sm.clients[name]; ok {
		return client, nil
	}

	// 从配置中查找服务器
	cfg, ok := sm.configs[name]
	if !ok {
		return nil, fmt.Errorf("服务器 '%s' 未在配置中找到", name)
	}

	// 构建 SSH 签名者
	var signer ssh.Signer
	var err error

	if cfg.KeyContent != "" {
		signer, err = ssh.ParsePrivateKey([]byte(cfg.KeyContent))
	} else if cfg.KeyPath != "" {
		key, readErr := os.ReadFile(cfg.KeyPath)
		if readErr != nil {
			return nil, fmt.Errorf("读取私钥文件失败 (%s): %w", cfg.KeyPath, readErr)
		}
		signer, err = ssh.ParsePrivateKey(key)
	} else {
		return nil, fmt.Errorf("服务器 '%s' 未提供 SSH 密钥（请设置 key_path 或 key_content）", name)
	}

	if err != nil {
		return nil, fmt.Errorf("解析 SSH 密钥失败: %w", err)
	}

	// 创建 SSH 配置
	sshConfig := &ssh.ClientConfig{
		User: cfg.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// 生产环境建议使用 ssh.HostKeyCallback(func(hostname string, remote string, key ssh.PublicKey) error { ... })
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// 创建客户端实例
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	client := &SSHClient{
		config: sshConfig,
		addr:   addr,
	}

	// 缓存连接（惰性初始化）
	sm.clients[name] = client
	return client, nil
}

// RunCommand 在远程服务器上执行命令
func (sc *SSHClient) RunCommand(cmd string) (string, error) {
	conn, err := ssh.Dial("tcp", sc.addr, sc.config)
	if err != nil {
		return "", fmt.Errorf("SSH 连接失败: %w", err)
	}
	defer conn.Close()

	session, err := conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("创建会话失败: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if err := session.Run(cmd); err != nil {
		return "", fmt.Errorf("命令执行失败: %w\n错误输出: %s", err, stderr.String())
	}

	return stdout.String(), nil
}

// ==================== 工具处理函数 ====================

// handleListServers 列出所有可用的服务器
func handleListServers(sm *ServerManager) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var sb strings.Builder
		sb.WriteString("可用的远程服务器列表:\n\n")

		for name, cfg := range sm.configs {
			sb.WriteString(fmt.Sprintf("**%s**\n", name))
			sb.WriteString(fmt.Sprintf("  - 地址: %s:%d\n", cfg.Host, cfg.Port))
			sb.WriteString(fmt.Sprintf("  - 用户: %s\n", cfg.User))
			if cfg.Description != "" {
				sb.WriteString(fmt.Sprintf("  - 说明: %s\n", cfg.Description))
			}
			sb.WriteString("\n")
		}

		return mcp.NewToolResultText(sb.String()), nil
	}
}

// handleExecuteCommand 在指定服务器上执行命令
func handleExecuteCommand(sm *ServerManager) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		serverName := req.GetString("server", "")
		if serverName == "" {
			return mcp.NewToolResultError("参数 'server' 是必填项，请从 list_servers 获取服务器名称"), nil
		}

		command := req.GetString("command", "")
		if command == "" {
			return mcp.NewToolResultError("参数 'command' 是必填项"), nil
		}

		client, err := sm.GetClient(serverName)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		output, err := client.RunCommand(command)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("[服务器: %s]\n%s", serverName, output)), nil
	}
}

// handleReadFile 读取远程文件
func handleReadFile(sm *ServerManager) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		serverName := req.GetString("server", "")
		if serverName == "" {
			return mcp.NewToolResultError("参数 'server' 是必填项"), nil
		}

		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("参数 'path' 是必填项（文件绝对路径）"), nil
		}

		client, err := sm.GetClient(serverName)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// 使用 cat 命令读取文件内容
		output, err := client.RunCommand(fmt.Sprintf("cat %q", path))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("[文件读取 - 服务器: %s]\n路径: %s\n\n内容:\n%s", serverName, path, output)), nil
	}
}

// handleWriteFile 写入远程文件（覆盖模式）
func handleWriteFile(sm *ServerManager) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		serverName := req.GetString("server", "")
		if serverName == "" {
			return mcp.NewToolResultError("参数 'server' 是必填项"), nil
		}

		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("参数 'path' 是必填项（文件绝对路径）"), nil
		}

		content := req.GetString("content", "")
		if content == "" {
			return mcp.NewToolResultError("参数 'content' 是必填项（要写入的内容）"), nil
		}

		client, err := sm.GetClient(serverName)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// 使用 Heredoc 方式写入，处理单引号防止 Shell 注入
		escapedContent := strings.ReplaceAll(content, "'", "'\\''")
		writeCmd := fmt.Sprintf("cat << 'MCP_EOF' > %q\n%s\nMCP_EOF", path, escapedContent)

		_, err = client.RunCommand(writeCmd)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("成功写入文件\n服务器: %s\n路径: %s\n大小: %d 字节", serverName, path, len(content))), nil
	}
}

// handleReadRemoteFile 通过工具调用读取本地配置文件中的文件（资源功能）
func handleReadConfigFile(sm *ServerManager) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("参数 'path' 是必填项"), nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("读取文件失败: %w", err)), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("文件内容:\n%s", string(data))), nil
	}
}

// ==================== 主函数 ====================

func main() {
	// 1. 加载 YAML 配置文件
	configData, err := os.ReadFile("config.yaml")
	if err != nil {
		// 如果配置文件不存在，尝试读取其他可能的文件名
		configData, err = os.ReadFile("config.yml")
		if err != nil {
			log.Fatalf("无法读取配置文件: %v\n请确保 config.yaml 存在于当前目录", err)
		}
	}

	var cfg Config
	if err := yaml.Unmarshal(configData, &cfg); err != nil {
		log.Fatalf("解析配置文件失败: %v\n请检查 YAML 格式是否正确", err)
	}

	if len(cfg.Servers) == 0 {
		log.Fatal("配置文件中没有找到任何服务器定义 (servers 节点为空)")
	}

	// 2. 初始化服务器管理器
	sm := NewServerManager(cfg)

	// 3. 创建 MCP Server
	mcpServer := server.NewMCPServer("Multi-SSH-Commander", "1.0.0")

	// 4. 注册工具

	// 工具 1: 列出所有可用服务器
	mcpServer.AddTool(mcp.NewTool("list_servers",
		mcp.WithDescription("列出所有已配置的远程服务器及其连接信息"),
	), handleListServers(sm))

	// 工具 2: 在指定服务器上执行命令
	mcpServer.AddTool(mcp.NewTool("execute_command",
		mcp.WithDescription("在指定的远程服务器上执行 Shell 命令并返回输出结果"),
		mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称（从 list_servers 获取）")),
		mcp.WithString("command", mcp.Required(), mcp.Description("要执行的 Shell 命令")),
	), handleExecuteCommand(sm))

	// 工具 3: 读取远程文件
	mcpServer.AddTool(mcp.NewTool("read_file",
		mcp.WithDescription("从指定的远程服务器读取文件内容"),
		mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称")),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件的绝对路径")),
	), handleReadFile(sm))

	// 工具 4: 写入远程文件
	mcpServer.AddTool(mcp.NewTool("write_file",
		mcp.WithDescription("向指定的远程服务器写入文件内容（会覆盖原有内容）"),
		mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称")),
		mcp.WithString("path", mcp.Required(), mcp.Description("目标文件的绝对路径")),
		mcp.WithString("content", mcp.Required(), mcp.Description("要写入的完整内容")),
	), handleWriteFile(sm))

	// 工具 5: 读取本地文件（用于查看配置、脚本等）
	mcpServer.AddTool(mcp.NewTool("read_local_file",
		mcp.WithDescription("读取本地机器上的文件内容（用于查看配置文件、脚本等本地资源）"),
		mcp.WithString("path", mcp.Required(), mcp.Description("本地文件的绝对路径")),
	), handleReadConfigFile(sm))

	// 5. 根据配置选择传输模式启动
	fmt.Fprintln(os.Stderr, "=================================================")
	fmt.Fprintln(os.Stderr, "  Multi-SSH MCP Server")
	fmt.Fprintf(os.Stderr, "  已加载 %d 个服务器配置\n", len(cfg.Servers))

	// 默认使用 stdio 模式（兼容 Claude Desktop 等本地客户端）
	transport := cfg.Transport
	if transport == "" {
		transport = "stdio"
	}
	// 环境变量覆盖（便于 run.sh stdio/http 一键切换）
	if envTransport := os.Getenv("MCP_TRANSPORT"); envTransport != "" {
		transport = envTransport
	}

	switch transport {
	case "http":
		startHTTPServer(mcpServer, cfg.HTTP, len(cfg.Servers))
	default:
		fmt.Fprintln(os.Stderr, "  传输模式: stdio")
		fmt.Fprintln(os.Stderr, "=================================================")
		if err := server.ServeStdio(mcpServer); err != nil {
			log.Fatalf("MCP Server 运行失败: %v", err)
		}
	}
}

// startHTTPServer 以 Streamable HTTP 模式启动（支持网页前端 / nginx 反代）
func startHTTPServer(mcpServer *server.MCPServer, httpCfg HTTPConfig, serverCount int) {
	host := httpCfg.Host
	if host == "" {
		host = "0.0.0.0"
	}
	port := httpCfg.Port
	if port == 0 {
		port = 8080
	}
	path := httpCfg.Path
	if path == "" {
		path = "/mcp"
	}

	// 构建 Streamable HTTP 服务（CORS 由我们自己的中间件处理，更灵活、可通配所有域名）
	sseSrv := server.NewStreamableHTTPServer(mcpServer, server.WithEndpointPath(path))

	// 鉴权中间件：校验 access_token，防止端点被未授权访问（仅 HTTP 模式生效）
	authHandler := withAuth(sseSrv, httpCfg.Auth)
	// 自定义 CORS 中间件包裹在最外层：确保 401 等响应也带 CORS 头，浏览器可读
	corsHandler := withCORS(authHandler, httpCfg)

	mux := http.NewServeMux()
	mux.Handle(path, corsHandler)
	// 健康检查端点，方便用 curl 快速验证服务存活（无需构造 MCP 协议请求）
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"ok","transport":"http","servers":%d}`, serverCount)))
	})

	addr := fmt.Sprintf("%s:%d", host, port)
	srv := &http.Server{Addr: addr, Handler: mux}

	fmt.Fprintln(os.Stderr, "  传输模式: http (Streamable HTTP)")
	fmt.Fprintf(os.Stderr, "  监听地址: %s\n", addr)
	fmt.Fprintf(os.Stderr, "  MCP 端点: %s\n", path)
	fmt.Fprintf(os.Stderr, "  健康检查: http://%s/health\n", addr)
	if len(httpCfg.CORSOrigins) > 0 {
		fmt.Fprintf(os.Stderr, "  CORS 源: %v\n", httpCfg.CORSOrigins)
	} else {
		fmt.Fprintln(os.Stderr, "  CORS: 未启用（若经 nginx 反代，请在 nginx 层配置跨域）")
	}
	if httpCfg.Auth.Enabled {
		fmt.Fprintf(os.Stderr, "  鉴权: 已启用（需携带 access_token，共 %d 个合法令牌）\n", len(resolveAuthTokens(httpCfg.Auth)))
	} else {
		fmt.Fprintln(os.Stderr, "  鉴权: 未启用（任何能访问端口者均可调用，建议内网/反代层再加保护）")
	}
	fmt.Fprintln(os.Stderr, "=================================================")

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("HTTP MCP Server 启动失败: %v", err)
	}
}

// withCORS 返回一个带 CORS 支持的 http.Handler。
// 相比 mcp-go 自带的 CORS，它的优势：
//   - cors_origins 包含 "*" 时允许任意域名（通配所有）
//   - 预检(OPTIONS)请求回显浏览器声明的所有请求头，自动放行 mcp-protocol-version 等自定义头
//   - 同源/无 Origin 请求（curl、nginx 内部转发）直接透传，不附加 CORS 头
func withCORS(next http.Handler, cfg HTTPConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// 非跨域请求（curl、同域、nginx 内部转发）直接透传
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		allowOrigin := resolveCORSOrigin(origin, cfg.CORSOrigins)
		if allowOrigin == "" {
			// 来源不在白名单，按普通请求处理（不附加 CORS 头）
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
		if cfg.CORSCredentials {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		// 暴露给浏览器的响应头（含会话 ID，前端必须能读取）
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, Content-Type")

		// 处理 CORS 预检请求（浏览器在发实际请求前会先发 OPTIONS）
		if r.Method == http.MethodOptions {
			// 回显浏览器在预检中声明的请求头，确保 mcp-protocol-version 等自定义头被放行
			reqHeaders := r.Header.Get("Access-Control-Request-Headers")
			if reqHeaders == "" {
				reqHeaders = "Content-Type, Authorization, Mcp-Session-Id, Accept, Mcp-Protocol-Version, Last-Event-ID"
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// resolveCORSOrigin 根据配置解析 Access-Control-Allow-Origin 的值。
// 返回空字符串表示来源不被允许。
func resolveCORSOrigin(origin string, allowed []string) string {
	if len(allowed) == 0 {
		return ""
	}
	// 通配：允许任意域名。若同时要求凭证，则回显具体来源（"*" 不能与凭证共存，规范限制）
	if contains(allowed, "*") {
		if origin == "" {
			return "*"
		}
		return origin
	}
	// 精确匹配白名单
	for _, a := range allowed {
		if a == origin {
			return origin
		}
	}
	return ""
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// resolveAuthTokens 解析配置中的令牌，支持 ${ENV_VAR} 从环境变量读取（避免明文写入 config.yaml）
func resolveAuthTokens(cfg AuthConfig) []string {
	var out []string
	for _, t := range cfg.Tokens {
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "${") && strings.HasSuffix(t, "}") {
			envVar := t[2 : len(t)-1]
			if v := os.Getenv(envVar); v != "" {
				out = append(out, v)
			}
			continue
		}
		out = append(out, t)
	}
	return out
}

// withAuth 返回一个带 access_token 鉴权的中间件。
//   - 仅校验实际请求（非 OPTIONS 预检），预检始终放行以完成 CORS 握手
//   - 客户端需通过以下任一方式提供令牌：
//     1. Authorization: Bearer <token>
//     2. X-Access-Token: <token>
//   - 未启用或令牌列表为空时直接透传（不鉴权）
func withAuth(next http.Handler, cfg AuthConfig) http.Handler {
	validTokens := make(map[string]bool)
	for _, t := range resolveAuthTokens(cfg) {
		validTokens[t] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 预检请求不做鉴权（浏览器预检不携带凭证，否则无法完成跨域握手）
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		// 未启用或没有合法令牌时直接放行
		if !cfg.Enabled || len(validTokens) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		token := extractToken(r)
		if validTokens[token] {
			next.ServeHTTP(w, r)
			return
		}

		// 鉴权失败：返回 401，并附带 JSON-RPC 风格错误信息
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"Unauthorized: invalid or missing access_token"},"id":null}`))
	})
}

// extractToken 从请求中提取 access_token（优先 Authorization: Bearer，其次 X-Access-Token 头）
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if strings.HasPrefix(h, "Bearer ") {
			return strings.TrimSpace(h[len("Bearer "):])
		}
		return strings.TrimSpace(h) // 兼容直接写裸 token 的情况
	}
	return strings.TrimSpace(r.Header.Get("X-Access-Token"))
}
