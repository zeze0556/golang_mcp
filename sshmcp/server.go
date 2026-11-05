package sshmcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ServerOption 配置 NewServer 的可选项。
type ServerOption func(*serverOptions)

type serverOptions struct {
	name                    string
	version                 string
	logger                  *slog.Logger
	allowAdditionalProperties bool
	audit                   bool
}

// WithName 设置 MCP Server 名称（默认 Multi-SSH-Commander）。
func WithName(name string) ServerOption {
	return func(o *serverOptions) { o.name = name }
}

// WithVersion 设置 MCP Server 版本（默认 1.0.0）。
func WithVersion(version string) ServerOption {
	return func(o *serverOptions) { o.version = version }
}

// WithLogger 注入结构化日志器（默认以 info 级别写 os.Stderr）。
func WithLogger(l *slog.Logger) ServerOption {
	return func(o *serverOptions) { o.logger = l }
}

// WithAllowAdditionalProperties 控制是否允许客户端传递未声明的额外工具参数（默认 false=严格）。
func WithAllowAdditionalProperties(b bool) ServerOption {
	return func(o *serverOptions) { o.allowAdditionalProperties = b }
}

// WithAuditLogging 启用工具调用审计日志中间件（记录 tool 名/参数/耗时/失败原因）。默认 false。
func WithAuditLogging(b bool) ServerOption {
	return func(o *serverOptions) { o.audit = b }
}

// NewServer 根据 ServerManager 构造一个已注册全部工具、可选携带审计日志的 MCP Server。
func NewServer(sm *ServerManager, opts ...ServerOption) *server.MCPServer {
	o := &serverOptions{
		name:    "Multi-SSH-Commander",
		version: "1.0.0",
		audit:   false,
	}
	for _, opt := range opts {
		opt(o)
	}

	if o.logger == nil {
		o.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	mcpServer := server.NewMCPServer(o.name, o.version, server.WithLogger(o.logger))
	if o.audit {
		mcpServer.Use(auditMiddleware(o.logger))
	}
	RegisterTools(mcpServer, sm, o.allowAdditionalProperties)
	return mcpServer
}

// resultPreviewLen 日志中单条返回文本的最大预览长度，防刷屏。
const resultPreviewLen = 2000

// truncateResult 从 CallToolResult 里取第一个 Content 文本的前 N 字符用于日志预览。
func truncateResult(r *mcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	for _, c := range r.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			s := tc.Text
			if len(s) > resultPreviewLen {
				s = s[:resultPreviewLen] + "...(truncated)"
			}
			return s
		}
	}
	return ""
}

// auditMiddleware 打印 tool 名、参数、耗时、失败原因，覆盖框架自带日志不记录 parameters 的缺口。
// 返回的是 mcp-go 的 ToolHandlerMiddleware（接收 next 并返回包装后的 handler）。
func auditMiddleware(l *slog.Logger) func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// DEBUG 模式：打印客户端发送的原始请求内容（含全部参数，可能含敏感信息，仅调试用）
			if l.Enabled(ctx, slog.LevelDebug) {
				if raw, mErr := json.Marshal(request); mErr == nil {
					l.Debug("mcp.tool.raw_request",
						slog.String("tool", request.Params.Name),
						slog.String("raw_request", string(raw)),
					)
				}
			}
			start := time.Now()
			result, err := next(ctx, request)
			dur := time.Since(start)
			// 提取关键参数用于日志（脱敏，不输出 host/port/user/key）
			serverName := request.GetString("server", "")
			reason := request.GetString("reason", "")
			lg := l.With(
				slog.String("tool", request.Params.Name),
				slog.String("server", serverName),
				slog.String("reason", reason), // 操作原因（审计记录）
				slog.Duration("duration", dur),
			)
			if err != nil {
				lg.Error("mcp.tool.failed", slog.String("error", err.Error()))
			} else {
				// 记录返回结果：文本内容截取前 2000 字符防刷屏，错误标记用 isError
				isError := result != nil && result.IsError
				lg.Info("mcp.tool.done", slog.Bool("is_error", isError),
					slog.String("result_preview", truncateResult(result)),
				)
			}
			return result, err
		}
	}
}
