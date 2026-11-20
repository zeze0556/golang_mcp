// 命令行入口：把 sshmcp 库装配成一个独立可运行的 Multi-SSH MCP 服务。
// 核心逻辑（连接池、工具、HTTP 传输、CORS、鉴权、审计）都在 github.com/zeze0556/golang_mcp/sshmcp 包内，
// 这里只负责读配置、拼装 MCP Server、选择 stdio / http 传输。
package main

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/zeze0556/golang_mcp/kb"
	sshmcp "github.com/zeze0556/golang_mcp/sshmcp"
)

func main() {
	// 1. 加载 YAML 配置文件（config.yaml 优先，回退 config.yml）
	configPath := "config.yaml"
	if _, err := os.Stat(configPath); err != nil {
		configPath = "config.yml"
		if _, e := os.Stat(configPath); e != nil {
			log.Fatalf("无法读取配置文件: 请确保 config.yaml 或 config.yml 存在于当前目录")
		}
	}

	cfg, err := sshmcp.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 2. 初始化服务器管理器（独立二进制无需热重载，cfgPath 传空）
	sm := sshmcp.NewServerManager(cfg, "")

	// 3. 日志级别由 MCP_LOG_LEVEL 环境变量控制
	logLevel := slog.LevelInfo
	switch strings.ToUpper(strings.TrimSpace(os.Getenv("MCP_LOG_LEVEL"))) {
	case "DEBUG":
		logLevel = slog.LevelDebug
	case "WARN", "WARNING":
		logLevel = slog.LevelWarn
	case "ERROR":
		logLevel = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))

	// 4. 构造 MCP Server（注册工具 + 审计日志 + 严格参数校验）
	mcpServer := sshmcp.NewServer(sm,
		sshmcp.WithLogger(logger),
		sshmcp.WithAuditLogging(true),
		sshmcp.WithAllowAdditionalProperties(cfg.ToolValidation.AllowAdditionalProperties),
	)

	// 4b. 知识库子系统（独立开关；enabled=false 时不注册任何 KB 工具、不监听目录）
	var kbSvc *kb.Service
	httpOpts := []sshmcp.HTTPServerOption{}
	if cfg.KnowledgeBase.Enabled {
		var err error
		kbSvc, err = kb.NewService(cfg.KnowledgeBase, logger)
		if err != nil {
			log.Fatalf("知识库初始化失败: %v", err)
		}
		defer kbSvc.Close()
		kb.RegisterTools(mcpServer, kbSvc)
		// 把 HTTP token 注入 MCP context，供 KB 工具做服务端 ACL（schema 不含 token）
		httpOpts = append(httpOpts, sshmcp.WithContextInjector(kb.WithToken))
		fmt.Fprintf(os.Stderr, "  知识库: 已启用（%d chunks）\n", kbSvc.Store().Count())
	} else {
		fmt.Fprintln(os.Stderr, "  知识库: 未启用")
	}

	fmt.Fprintln(os.Stderr, "=================================================")
	fmt.Fprintln(os.Stderr, "  Multi-SSH MCP Server")
	fmt.Fprintf(os.Stderr, "  已加载 %d 个服务器配置\n", len(cfg.Servers))

	// 5. 根据配置选择传输模式启动
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
		if err := sshmcp.StartHTTPServer(mcpServer, cfg.HTTP, len(cfg.Servers), httpOpts...); err != nil {
			log.Fatalf("MCP Server 运行失败: %v", err)
		}
	default:
		fmt.Fprintln(os.Stderr, "  传输模式: stdio")
		fmt.Fprintln(os.Stderr, "=================================================")
		if err := sshmcp.ServeStdio(mcpServer); err != nil {
			log.Fatalf("MCP Server 运行失败: %v", err)
		}
	}
}
