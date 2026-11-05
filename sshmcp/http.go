package sshmcp

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/server"
)

// BuildCORSOptions 将 HTTPConfig 映射为 mcp-go 库内置的 CORS 选项。
// 仅在配置了 cors_origins 时返回非 nil（即启用 CORS）；未配置则 nil（不启用 CORS，由反代/同域处理跨域）。
func BuildCORSOptions(cfg HTTPConfig) []server.CORSOption {
	if len(cfg.CORSOrigins) == 0 {
		return nil
	}
	opts := []server.CORSOption{
		server.WithCORSAllowedOrigins(cfg.CORSOrigins...),
		server.WithCORSAllowedMethods("GET", "POST", "OPTIONS"),
		// 放行 MCP 所需的自定义请求头
		server.WithCORSAllowedHeaders(
			"Content-Type", "Authorization", "Mcp-Session-Id",
			"Accept", "Mcp-Protocol-Version", "Last-Event-ID",
		),
		// 暴露会话 ID 等响应头，前端必须能读取
		server.WithCORSExposedHeaders("Mcp-Session-Id", "Content-Type"),
		server.WithCORSMaxAge(86400),
	}
	if cfg.CORSCredentials {
		opts = append(opts, server.WithCORSAllowCredentials())
	}
	return opts
}

// buildStreamableServer 构造带库内置 CORS 的 Streamable HTTP 服务（不含鉴权）。
func buildStreamableServer(mcpServer *server.MCPServer, httpCfg HTTPConfig) *server.StreamableHTTPServer {
	path := httpCfg.Path
	if path == "" {
		path = "/mcp"
	}
	streamOpts := []server.StreamableHTTPOption{
		server.WithEndpointPath(path),
	}
	if corsOpts := BuildCORSOptions(httpCfg); corsOpts != nil {
		streamOpts = append(streamOpts, server.WithStreamableHTTPCORS(corsOpts...))
	}
	return server.NewStreamableHTTPServer(mcpServer, streamOpts...)
}

// BuildHTTPHandler 返回 MCP 端点完整 HTTP 处理链：Streamable 服务（含库内置 CORS）外层包静态令牌鉴权。
// 宿主程序可直接将返回值注册到任意 *http.ServeMux 的端点路径上。
func BuildHTTPHandler(mcpServer *server.MCPServer, httpCfg HTTPConfig) http.Handler {
	streamableSrv := buildStreamableServer(mcpServer, httpCfg)
	if httpCfg.Auth.Enabled {
		return AuthMiddleware(streamableSrv, httpCfg)
	}
	return streamableSrv
}

// AuthMiddleware 返回一个带 access_token 鉴权的中间件。
//   - 仅校验实际请求（非 OPTIONS 预检），预检始终放行以完成 CORS 握手
//   - 客户端需通过以下任一方式提供令牌：
//     1. Authorization: Bearer <token>
//     2. X-Access-Token: <token>
//   - 未启用或令牌列表为空时直接透传（不鉴权）
//   - 401 响应会回显允许源的 CORS 头：因 MCP 端点的 CORS 已由 mcp-go 库内置（位于 streamable server 内层），
//     鉴权失败产生的 401 默认不带 CORS 头，这里补回显以保证跨域浏览器仍能读取 401。
func AuthMiddleware(next http.Handler, cfg HTTPConfig) http.Handler {
	validTokens := make(map[string]bool)
	for _, t := range resolveAuthTokens(cfg.Auth) {
		validTokens[t] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 预检请求不做鉴权（浏览器预检不携带凭证，否则无法完成跨域握手）
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		// 未启用或没有合法令牌时直接放行
		if !cfg.Auth.Enabled || len(validTokens) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		token := extractToken(r)
		if validTokens[token] {
			next.ServeHTTP(w, r)
			return
		}

		// 鉴权失败：返回 401，并附带 JSON-RPC 风格错误信息
		// 补回 CORS 头（仅对允许的来源），保证跨域浏览器可读到 401。
		if allow := resolveCORSOrigin(r.Header.Get("Origin"), cfg.CORSOrigins); allow != "" {
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Origin", allow)
			if cfg.CORSCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"Unauthorized: invalid or missing access_token"},"id":null}`))
	})
}

// CORSJSONMiddleware 返回一个带 CORS 支持的 http.Handler，用于非 MCP 端点（如管理/健康检查）。
// 相比 mcp-go 自带的 CORS，它的优势：
//   - cors_origins 包含 "*" 时允许任意域名（通配所有）
//   - 预检(OPTIONS)请求回显浏览器声明的所有请求头，自动放行 mcp-protocol-version 等自定义头
//   - 同源/无 Origin 请求（curl、nginx 内部转发）直接透传，不附加 CORS 头
//
// 典型用法：CORSJSONMiddleware(AuthMiddleware(h, cfg), cfg) 组合出受保护与跨域的管理端点。
func CORSJSONMiddleware(next http.Handler, cfg HTTPConfig) http.Handler {
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

// resolveCORSOrigin 根据配置解析允许访问的源（用于 auth 层 401 响应补 CORS 头，以及 CORSJSONMiddleware）。
// 返回空字符串表示来源不被允许。库内置 CORS 已处理正常请求，此函数仅服务于 401/管理端点等边缘场景。
func resolveCORSOrigin(origin string, allowed []string) string {
	if origin == "" || len(allowed) == 0 {
		return ""
	}
	if contains(allowed, "*") {
		return origin
	}
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

// ServeStdio 以 stdio 模式启动 MCP 服务（兼容 Claude Desktop 等本地客户端）。
func ServeStdio(mcpServer *server.MCPServer) error {
	return server.ServeStdio(mcpServer)
}

// StartHTTPServer 以 Streamable HTTP 模式启动（支持网页前端 / nginx 反代）。
// 在 path 暴露 MCP 端点，并额外提供 /health 健康检查。本函数会阻塞监听，一般不会返回（除非监听失败）。
func StartHTTPServer(mcpServer *server.MCPServer, httpCfg HTTPConfig, serverCount int) error {
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

	handler := BuildHTTPHandler(mcpServer, httpCfg)

	mux := http.NewServeMux()
	mux.Handle(path, handler)
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
		return fmt.Errorf("HTTP MCP Server 启动失败: %w", err)
	}
	return nil
}
