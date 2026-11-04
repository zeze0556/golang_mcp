package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
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
	// InsecureIgnoreHostKey 为 true 时关闭 SSH 主机密钥校验（等价于 ssh.InsecureIgnoreHostKey，存在中间人风险）。
	// 默认 false：走 known_hosts 校验，校验失败（未知/变更主机）将拒绝连接。
	InsecureIgnoreHostKey bool `yaml:"insecure_ignore_host_key,omitempty"`
}

// ToolValidation MCP 工具参数校验配置
type ToolValidation struct {
	AllowAdditionalProperties bool `yaml:"allow_additional_properties,omitempty"` // 是否允许客户端传递未声明的额外参数，默认 false（严格模式）
}

// CommandConfig 命令执行相关的安全/健壮性配置
type CommandConfig struct {
	// TimeoutSeconds 单条命令的最大执行时间（秒）。默认 120；设为 0 表示不限时（不推荐）。
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
	// MaxOutputMB 单条命令 stdout+stderr 的采集上限（MB）。默认 4；超出部分截断，防止超大输出撑爆响应。
	MaxOutputMB int `yaml:"max_output_mb,omitempty"`
}

// LocalReadConfig 本地文件读取（read_local_file 工具）的访问控制
// 出于安全考虑，该工具默认关闭；开启后仅允许读取 allowed_dirs 指定目录下的文件，
// 避免被（即便持有 token 的）调用方读取 SSH 私钥、config.yaml 等敏感本地文件。
type LocalReadConfig struct {
	Enabled     bool     `yaml:"enabled,omitempty"`
	AllowedDirs []string `yaml:"allowed_dirs,omitempty"` // 允许读取的本地目录白名单（绝对路径）
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
	Transport       string                 `yaml:"transport,omitempty"` // stdio 或 http，默认 stdio
	HTTP            HTTPConfig             `yaml:"http,omitempty"`      // HTTP 模式配置
	Servers         map[string]SSHConfig   `yaml:"servers"`
	ToolValidation  ToolValidation         `yaml:"tool_validation,omitempty"` // 工具参数校验配置
	Command         CommandConfig          `yaml:"command,omitempty"`          // 命令执行超时/输出上限
	LocalRead       LocalReadConfig        `yaml:"local_read,omitempty"`       // 本地文件读取白名单
	// KnownHostsPath 全局 known_hosts 文件路径；为空时回退到 ~/.ssh/known_hosts。
	// 仅当所有服务器都未设置 insecure_ignore_host_key 时生效。
	KnownHostsPath  string                 `yaml:"known_hosts_path,omitempty"`
}

// SSHClient 封装 SSH 连接（连接池：复用单条长连接，避免每次调用都重建 SSH 握手）
type SSHClient struct {
	mu         sync.Mutex
	config     *ssh.ClientConfig
	addr       string
	client     *ssh.Client  // 持久 SSH 连接，懒建立、可重连；nil 表示需重建
	sftpClient *sftp.Client // 与 client 绑定的 SFTP 子系统句柄，随连接复用；nil 表示需重建
}

// ServerManager 管理多个 SSH 连接
type ServerManager struct {
	mu      sync.RWMutex
	configs map[string]SSHConfig
	clients map[string]*SSHClient

	// 运行时参数（由配置解析而来，全局统一）
	cmdTimeout time.Duration // 单条命令执行超时
	maxOutput  int           // 单条命令输出采集上限（字节）
	knownHosts string        // known_hosts 文件路径
	localRead  LocalReadConfig
}

// ==================== SSH 客户端逻辑 ====================

// NewServerManager 从配置创建服务器管理器
func NewServerManager(cfg Config) *ServerManager {
	cmdTimeout := time.Duration(cfg.Command.TimeoutSeconds) * time.Second
	if cfg.Command.TimeoutSeconds <= 0 {
		cmdTimeout = 120 * time.Second
	}
	maxOutput := cfg.Command.MaxOutputMB * 1024 * 1024
	if cfg.Command.MaxOutputMB <= 0 {
		maxOutput = 4 * 1024 * 1024
	}
	return &ServerManager{
		configs:    cfg.Servers,
		clients:    make(map[string]*SSHClient),
		cmdTimeout: cmdTimeout,
		maxOutput:  maxOutput,
		knownHosts: cfg.KnownHostsPath,
		localRead:  cfg.LocalRead,
	}
}

// buildHostKeyCallback 构造 SSH 主机密钥校验回调。
//   - insecure=true：直接返回 InsecureIgnoreHostKey（跳过校验，存在中间人风险）
//   - insecure=false（默认）：使用 known_hosts 进行严格校验；文件不存在/不可读时
//     返回一个失败关闭的回调，强制用户先准备 known_hosts 或显式 insecure_ignore_host_key: true
func buildHostKeyCallback(knownHostsPath string, insecure bool) ssh.HostKeyCallback {
	if insecure {
		return ssh.InsecureIgnoreHostKey()
	}
	path := knownHostsPath
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".ssh", "known_hosts")
		}
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return fmt.Errorf("无法加载 known_hosts (%s)：%w；请先生成 known_hosts（如 ssh-keyscan 目标主机），或为该服务器显式设置 insecure_ignore_host_key: true", path, err)
		}
	}
	return cb
}

// GetClient 获取或创建 SSH 连接（单例模式，并发安全）
func (sm *ServerManager) GetClient(name string) (*SSHClient, error) {
	// 快速路径：已有缓存连接，直接返回（读锁，并发安全）
	sm.mu.RLock()
	client, ok := sm.clients[name]
	sm.mu.RUnlock()
	if ok {
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
		// 默认走 known_hosts 严格校验；仅当该服务器显式 insecure_ignore_host_key: true 时才跳过。
		HostKeyCallback: buildHostKeyCallback(sm.knownHosts, cfg.InsecureIgnoreHostKey),
		Timeout:         sshHandshakeTimeout, // 建连超时，避免握手卡死
	}

	// 创建客户端实例
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	newClient := &SSHClient{
		config: sshConfig,
		addr:   addr,
	}

	// 写锁 + 双重检查：避免并发 map 写入与重复建连
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if client, ok := sm.clients[name]; ok {
		return client, nil
	}
	sm.clients[name] = newClient
	return newClient, nil
}

// ==================== SSH 连接池 ====================
// 原实现每次 RunCommand 都 ssh.Dial 新建连接，付出一次完整 SSH 握手开销；
// 文件读写/命令执行频繁时，握手成本成为主要延迟来源。
// 改造：每服务器复用单条持久连接（懒建立、后台保活、传输层断开时重连一次并重试）。

const (
	sshHandshakeTimeout  = 15 * time.Second // 建连（TCP+KEX+认证）超时，避免握手卡死
	sshKeepAliveInterval = 30 * time.Second // 保活探测间隔
)

// getConn 返回一条可用的持久连接；不存在或已被丢弃则（重）建立。
// 仅在此函数内加锁做建连决策，命令执行在锁外进行，避免串行化同服务器的并发调用。
func (sc *SSHClient) getConn() (*ssh.Client, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if sc.client != nil {
		return sc.client, nil
	}

	conn, err := ssh.Dial("tcp", sc.addr, sc.config)
	if err != nil {
		return nil, fmt.Errorf("SSH 连接失败: %w", err)
	}
	sc.client = conn
	sc.sftpClient = nil // 新连接尚未开启 SFTP；旧 sftpClient 已随旧连接失效
	go sc.keepAliveLoop(conn) // 后台保活；连接被替换/关闭时该 goroutine 自行退出
	return conn, nil
}

// dropConn 仅在当前缓存的正是 c 时才置空并关闭，避免并发下误关其他连接。
func (sc *SSHClient) dropConn(c *ssh.Client) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.client == c {
		sc.client = nil
	}
	if sc.sftpClient != nil {
		_ = sc.sftpClient.Close() // 底层连接已失效，绑定的 SFTP 句柄同步回收
		sc.sftpClient = nil
	}
	_ = c.Close()
}

// connIsAlive 通过 OpenSSH 保活全局请求探测连接是否仍可用。
func (sc *SSHClient) connIsAlive(c *ssh.Client) bool {
	_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}

// keepAliveLoop 周期性发送保活探测；本 goroutine 持有的 conn 已被替换或连接断开时退出。
func (sc *SSHClient) keepAliveLoop(conn *ssh.Client) {
	ticker := time.NewTicker(sshKeepAliveInterval)
	defer ticker.Stop()
	for range ticker.C {
		sc.mu.Lock()
		stillMine := sc.client == conn
		sc.mu.Unlock()
		if !stillMine {
			return // 已被 redial 替换，旧连接由 dropConn 关闭
		}
		if _, _, err := conn.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			sc.dropConn(conn)
			return
		}
	}
}

// cappedBuffer 带上限的写入缓冲：超过 limit 字节后停止写入并标记 truncated，
// 用于限制单条命令的 stdout/stderr 采集量，防止超大输出撑爆响应/内存。
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.limit <= 0 {
		return c.buf.Write(p)
	}
	if c.buf.Len() >= c.limit {
		c.truncated = true
		return len(p), nil
	}
	room := c.limit - c.buf.Len()
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// errCmdAborted 表示命令因超时或客户端取消而被中断（非传输层错误，不应重连重试）。
var errCmdAborted = errors.New("command aborted by timeout or cancellation")

// RunCommand 在远程服务器上执行命令（复用持久连接）。
// ctx 用于取消/超时：超时或被客户端取消时会关闭会话（进而通知远端终止进程）；timeout/maxOutput 控制执行时限与输出上限。
// 若命令因传输层断开而失败，会重连一次并重试；仅当连接确已死亡才重试，
// 命令自身非零退出、或超时/取消都不会触发重试，避免对任意命令做 at-least-once 的重复执行。
func (sc *SSHClient) RunCommand(ctx context.Context, cmd string, timeout time.Duration, maxOutput int) (string, error) {
	conn, err := sc.getConn()
	if err != nil {
		return "", err
	}

	output, err := sc.runOnConn(ctx, conn, cmd, timeout, maxOutput)
	if err != nil {
		// 仅传输层错误（连接已死）才重连重试；超时/取消（errCmdAborted）不重试。
		if !errors.Is(err, errCmdAborted) && !sc.connIsAlive(conn) {
			sc.dropConn(conn)
			if conn2, derr := sc.getConn(); derr == nil {
				if out2, rerr := sc.runOnConn(ctx, conn2, cmd, timeout, maxOutput); rerr == nil {
					return out2, nil
				}
			}
		}
		return "", err
	}
	return output, nil
}

// runOnConn 在给定连接上开会话执行命令，返回 stdout；非零退出时把 stderr 并入错误。
// 通过 goroutine + select 在 ctx 超时/取消时关闭会话以中断远端进程；输出受 maxOutput 限制。
func (sc *SSHClient) runOnConn(ctx context.Context, conn *ssh.Client, cmd string, timeout time.Duration, maxOutput int) (string, error) {
	session, err := conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("创建会话失败: %w", err)
	}
	defer session.Close()

	var stdout, stderr cappedBuffer
	stdout.limit = maxOutput
	stderr.limit = maxOutput
	session.Stdout = &stdout
	session.Stderr = &stderr

	// 在调用方 ctx 之上叠加命令级超时；无 timeout 时仍随调用方 ctx 取消。
	runCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- session.Run(cmd) }()

	select {
	case runErr := <-done:
		if runErr != nil {
			msg := fmt.Sprintf("命令执行失败: %v", runErr)
			if stderr.truncated {
				msg += fmt.Sprintf("（stderr 超过 %d 字节已截断）", maxOutput)
			}
			return "", fmt.Errorf("%s\n错误输出: %s", msg, stderr.String())
		}
		out := stdout.String()
		if stdout.truncated {
			out += fmt.Sprintf("\n... (输出超过 %d 字节已截断，请加过滤条件或将结果重定向到文件后用 read_file 读取)", maxOutput)
		}
		return out, nil
	case <-runCtx.Done():
		// 超时/取消：关闭会话以通知远端终止进程（连接缓存保留，供后续调用复用）。
		_ = session.Close()
		return "", fmt.Errorf("%w: 时限 %v: %v", errCmdAborted, timeout, runCtx.Err())
	}
}

// openSFTP 返回与当前持久 SSH 连接绑定的 SFTP 客户端。
// 复用同一个 SFTP 子系统句柄（pkg/sftp 的 Client 内部做 multiplexing，可并发安全使用），
// 避免每次 read/write 都重新开启 SFTP 子系统（省去每次的 subsystem 请求 + 版本握手往返）。
// 句柄随底层 SSH 连接一起失效：getConn 重建连接、dropConn 丢弃连接时都会同步清空 sftpClient。
func (sc *SSHClient) openSFTP() (*sftp.Client, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.client == nil {
		return nil, fmt.Errorf("SSH 连接尚未建立")
	}
	if sc.sftpClient != nil {
		return sc.sftpClient, nil // 复用已开启的句柄
	}
	scli, err := sftp.NewClient(sc.client)
	if err != nil {
		return nil, fmt.Errorf("SFTP 子系统启动失败: %w", err)
	}
	sc.sftpClient = scli // 懒建立后缓存，后续调用直接复用
	return scli, nil
}

// ==================== 工具处理函数 ====================

// handleListServers 列出所有可用的服务器
// truncateResult 从 CallToolResult 里取第一个 Content 文本的前 N 字符用于日志预览
const resultPreviewLen = 2000

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

func handleListServers(sm *ServerManager) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// 按名称排序，保证结果稳定（map 遍历顺序随机）
		names := make([]string, 0, len(sm.configs))
		for name := range sm.configs {
			names = append(names, name)
		}
		sort.Strings(names)

		type srvInfo struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
		}
		infos := make([]srvInfo, 0, len(names))
		var sb strings.Builder
		sb.WriteString("可用的远程服务器列表:\n\n")
		for _, name := range names {
			cfg := sm.configs[name]
			infos = append(infos, srvInfo{Name: name, Description: cfg.Description})
			sb.WriteString(fmt.Sprintf("**- %s**", name))
			if cfg.Description != "" {
				sb.WriteString(fmt.Sprintf(": %s", cfg.Description))
			}
			sb.WriteString("\n")
		}

		// 同时返回结构化内容（供程序化消费）与文本回退（供模型阅读）
		structured := map[string]any{
			"count":    len(infos),
			"servers":  infos,
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				mcp.TextContent{Type: "text", Text: sb.String()},
			},
			StructuredContent: structured,
		}, nil
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

		output, err := client.RunCommand(ctx, command, sm.cmdTimeout, sm.maxOutput)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("[服务器: %s]\n%s", serverName, output)), nil
	}
}

// handleReadFile 读取远程文件（SFTP 子系统：随机字节读，无 shell/base64 开销）。
// 两种分页互斥：提供 offset_lines/limit_lines 任一即进入「行号分页」，否则走「字节分页」。
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

		// 字节分页（原语义）
		offset := req.GetInt("offset", 0)
		limit := req.GetInt("limit", 0)
		if offset < 0 {
			offset = 0
		}
		if limit < 0 {
			limit = 0
		}
		// 行号分页（新增）：offset_lines/limit_lines 任一 >0 即进入行模式
		offsetLines := req.GetInt("offset_lines", 0)
		limitLines := req.GetInt("limit_lines", 0)
		useLineMode := offsetLines > 0 || limitLines > 0

		client, err := sm.GetClient(serverName)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// 复用持久 SSH 连接上的 SFTP 子系统句柄（已池化，无需每次重建）
		scli, err := client.openSFTP()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("%v（请确认远程 sshd 已启用 sftp 子系统）", err)), nil
		}

		// 打开并按需取大小（一次 sftp 调用，替代原 shell stat/wc 的额外往返）
		f, err := scli.Open(path)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("打开文件失败: %v", err)), nil
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("无法访问文件 %s: %v", path, err)), nil
		}
		size := fi.Size()

		if useLineMode {
			return readFileLines(f, size, serverName, path, offsetLines, limitLines)
		}

		// ===== 字节模式 =====
		const maxReadDefault = 4 * 1024 * 1024 // 4MB：未指定 limit 时允许直接读取的上限
		if limit == 0 && size > maxReadDefault {
			return mcp.NewToolResultError(fmt.Sprintf("文件过大（约 %.1f MB），超过单次读取上限。请使用 offset/limit 分段读取，或使用 offset_lines/limit_lines 行号分页", float64(size)/float64(1024*1024))), nil
		}

		// 计算读取窗口并随机读（ReadAt 天然支持大文件、无需整文件进内存）
		start := int64(offset)
		end := size
		if limit > 0 {
			end = start + int64(limit)
		}
		if end > size {
			end = size
		}
		if start > end {
			start = end
		}
		n := end - start
		if n <= 0 {
			return mcp.NewToolResultText(fmt.Sprintf("[文件读取 - 服务器: %s]\n路径: %s\n偏移: %d\n字节数: 0\n\n（已到文件末尾或偏移越界）", serverName, path, offset)), nil
		}

		buf := make([]byte, n)
		m, rerr := readExactAt(f, buf, start)
		buf = buf[:m]
		if rerr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("读取文件失败: %v", rerr)), nil
		}

		// 二进制降级：含非法 UTF-8 时 base64 返回，防止 JSON 序列化损坏
		if !utf8.Valid(buf) {
			b64 := base64.StdEncoding.EncodeToString(buf)
			return mcp.NewToolResultText(fmt.Sprintf("[文件读取 - 二进制/base64]\n服务器: %s\n路径: %s\n偏移: %d\n字节数: %d\n\n%s", serverName, path, offset, len(buf), b64)), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("[文件读取 - 服务器: %s]\n路径: %s\n偏移: %d\n字节数: %d\n\n内容:\n%s", serverName, path, offset, len(buf), string(buf))), nil
	}
}

// readFileLines 行号分页：受 maxLineRead 上限读入后按换行切片，返回指定行范围（1-based）。
// 面向"很多行的文件"——模型按行思考，无需知道字节偏移即可精准取某几行。
// 注意：行模式需把目标区段读入内存，故设 32MB 上限；GB 级大文件请用 offset/limit 字节模式。
func readFileLines(f *sftp.File, size int64, serverName, path string, offsetLines, limitLines int) (*mcp.CallToolResult, error) {
	const maxLineRead = 32 * 1024 * 1024 // 32MB：行模式允许读入的最大字节
	if size > maxLineRead {
		return mcp.NewToolResultError(fmt.Sprintf("文件过大（约 %.1f MB），行号分页不支持。请改用 offset/limit 字节分页", float64(size)/float64(1024*1024))), nil
	}

	buf := make([]byte, size)
	m, rerr := readExactAt(f, buf, 0)
	buf = buf[:m]
	if rerr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("读取文件失败: %v", rerr)), nil
	}
	if !utf8.Valid(buf) {
		return mcp.NewToolResultError("文件疑似二进制内容，行号分页无效。请使用 offset/limit 字节模式读取"), nil
	}

	// 按行切分（1-based 行号），并去掉末尾换行带来的空行幻影
	text := string(buf)
	parts := strings.Split(text, "\n")
	total := len(parts)
	if total > 0 && parts[total-1] == "" && (len(text) == 0 || text[len(text)-1] == '\n') {
		total--
		parts = parts[:total]
	}

	// 计算返回范围
	startIdx := offsetLines - 1
	if startIdx < 0 {
		startIdx = 0
	}
	if startIdx > total {
		startIdx = total
	}
	endIdx := total
	if limitLines > 0 {
		endIdx = startIdx + limitLines
		if endIdx > total {
			endIdx = total
		}
	}
	if startIdx > endIdx {
		startIdx = endIdx
	}
	selected := parts[startIdx:endIdx]
	result := strings.Join(selected, "\n")

	return mcp.NewToolResultText(fmt.Sprintf("[文件读取-行号分页 服务器: %s]\n路径: %s\n总行数: %d\n返回行: %d-%d（共 %d 行）\n\n内容:\n%s",
		serverName, path, total, startIdx+1, endIdx, endIdx-startIdx, result)), nil
}

// handleWriteFile 写入远程文件（SFTP 子系统：流式分块写，无 base64 膨胀、无单命令长度上限）。
// 支持 append/offset 分块写大文件；单次仍受 MCP/HTTP body（nginx 10MB）约束，超请分块多调用。
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

		appendMode := req.GetBool("append", false)
		rawOffset := req.GetInt("offset", 0)
		writeOffset := int64(rawOffset)
		if writeOffset < 0 {
			writeOffset = 0
		}

		// 单次调用内容上限：受 MCP 传输层（HTTP body / nginx 10MB、stdio 内存）约束，
		// 超出请分块多次调用（首块 append=false 覆盖，后续 append=true + offset 续写）。
		const maxWrite = 8 * 1024 * 1024 // 8MB
		if len(content) > maxWrite {
			return mcp.NewToolResultError(fmt.Sprintf("写入内容过大（约 %.1f MB），超过单次调用上限。请分块写入：首块 append=false，后续 append=true 并指定 offset", float64(len(content))/float64(1024*1024))), nil
		}

		client, err := sm.GetClient(serverName)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		scli, err := client.openSFTP()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("%v（请确认远程 sshd 已启用 sftp 子系统）", err)), nil
		}

		// 流式分块写（内部按 4MB 切片多次 WriteAt，避免单次超大 Write 与内存尖峰）
		written, werr := sftpWriteAll(scli, path, []byte(content), appendMode, writeOffset, 4*1024*1024)
		if werr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("写入文件失败（已写 %d 字节）: %v", written, werr)), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("成功写入文件\n服务器: %s\n路径: %s\n模式: %s\n大小: %d 字节", serverName, path, modeDesc(appendMode), written)), nil
	}
}

// sftpWriteAll 通过 SFTP 把 data 分块流式写入 path：覆盖或追加，按 chunkSize 切片多次 WriteAt，
// 避免单次超大 Write 与内存尖峰。writeOffset<0 且 append 时追加到文件末尾；非 append 时从 0 开始。
func sftpWriteAll(scli *sftp.Client, path string, data []byte, appendMode bool, writeOffset int64, chunkSize int) (int, error) {
	mode := os.O_WRONLY | os.O_CREATE
	if !appendMode {
		mode |= os.O_TRUNC
	}
	f, err := scli.OpenFile(path, mode)
	if err != nil {
		return 0, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	off := writeOffset
	if appendMode {
		if writeOffset < 0 {
			if fi, e := scli.Stat(path); e == nil {
				off = fi.Size()
			} else {
				off = 0
			}
		}
	} else if off < 0 {
		off = 0
	}

	if chunkSize <= 0 {
		chunkSize = 4 * 1024 * 1024
	}
	written := 0
	for written < len(data) {
		end := written + chunkSize
		if end > len(data) {
			end = len(data)
		}
		w, werr := f.WriteAt(data[written:end], off+int64(written))
		written += w
		if werr != nil {
			return written, fmt.Errorf("写入中断（已写 %d 字节）: %w", written, werr)
		}
		if w == 0 {
			return written, fmt.Errorf("写入中断（已写 %d 字节，未继续）", written)
		}
	}
	return written, nil
}

// handleWriteFileChunked 分块上传助手：调用方传入完整 content，工具内部按 chunk_size
// 自动切片并以 SFTP WriteAt 落盘（调用方无需计算偏移）。支持 append 模式以支撑跨多次调用
// 上传超过单次 MCP/HTTP body（nginx 10MB）限制的大文件：首调用 append=false 覆盖，后续
// append=true 续写（本工具在 append 时自动定位到文件末尾，无需调用方算偏移）。
func handleWriteFileChunked(sm *ServerManager) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

		appendMode := req.GetBool("append", false)
		chunkSize := req.GetInt("chunk_size", 0)
		if chunkSize < 0 {
			chunkSize = 0
		}

		// 单次调用 content 仍受 MCP/HTTP body（nginx 10MB）约束；超出请拆成多块多次调用，
		// 首块 append=false，后续 append=true。
		const maxWrite = 8 * 1024 * 1024
		if len(content) > maxWrite {
			return mcp.NewToolResultError(fmt.Sprintf("单次 content 过大（约 %.1f MB），超过上限。请拆分为多块多次调用：首块 append=false，后续 append=true", float64(len(content))/float64(1024*1024))), nil
		}

		client, err := sm.GetClient(serverName)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		scli, err := client.openSFTP()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("%v（请确认远程 sshd 已启用 sftp 子系统）", err)), nil
		}

		// 分块上传助手：自动切片写入；append 时自动定位到末尾（writeOffset=-1）
		written, werr := sftpWriteAll(scli, path, []byte(content), appendMode, -1, chunkSize)
		if werr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("分块写入失败: %v", werr)), nil
		}

		// 回执给出当前文件大小（append 时为续写起点），便于调用方后续追加
		nextOffset := int64(written)
		if appendMode {
			if fi, e := scli.Stat(path); e == nil {
				nextOffset = fi.Size()
			}
		}
		return mcp.NewToolResultText(fmt.Sprintf("成功分块写入文件\n服务器: %s\n路径: %s\n模式: %s\n本次写入: %d 字节\n当前文件大小: %d",
			serverName, path, modeDesc(appendMode), written, nextOffset)), nil
	}
}

// readExactAt 从指定偏移循环读取，直到填满 buf 或遇到错误/EOF。
func readExactAt(f *sftp.File, buf []byte, off int64) (int, error) {
	total := 0
	for total < len(buf) {
		m, err := f.ReadAt(buf[total:], off+int64(total))
		total += m
		if err != nil {
			if err == io.EOF {
				break
			}
			return total, err
		}
		if m == 0 {
			break
		}
	}
	return total, nil
}

// modeDesc 将追加/覆盖模式转成中文描述，用于回执展示。
func modeDesc(appendMode bool) string {
	if appendMode {
		return "追加"
	}
	return "覆盖"
}

// handleReadConfigFile 读取本地机器上的文件内容。
// 出于安全考虑，默认不启用；启用后仅允许访问 allowed_dirs 下的文件，
// 避免被（即便持有 token 的）调用方读取 SSH 私钥、config.yaml 等敏感本地文件。
func handleReadConfigFile(sm *ServerManager) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if !sm.localRead.Enabled {
			return mcp.NewToolResultError("本地文件读取未启用（请在 config.yaml 中设置 local_read.enabled: true）"), nil
		}
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("参数 'path' 是必填项"), nil
		}

		clean := filepath.Clean(path)
		allowed := false
		for _, dir := range sm.localRead.AllowedDirs {
			base := filepath.Clean(dir)
			rel, err := filepath.Rel(base, clean)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != string(filepath.Separator) {
				allowed = true
				break
			}
		}
		if !allowed {
			return mcp.NewToolResultError(fmt.Sprintf("路径 %s 不在允许目录（local_read.allowed_dirs）内，已拒绝", path)), nil
		}

		data, err := os.ReadFile(clean)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("读取文件失败: %v", err)), nil
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

	// 3. 创建 MCP Server（携带结构化日志，记录客户端真实请求）
	//     日志级别由 MCP_LOG_LEVEL 环境变量控制：DEBUG 会额外打印客户端发送的原始请求内容（含全部参数，可能含敏感信息，仅调试用）
	logLevel := slog.LevelInfo
	switch strings.ToUpper(strings.TrimSpace(os.Getenv("MCP_LOG_LEVEL"))) {
	case "DEBUG":
		logLevel = slog.LevelDebug
	case "WARN", "WARNING":
		logLevel = slog.LevelWarn
	case "ERROR":
		logLevel = slog.LevelError
	}
	slogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))
	mcpServer := server.NewMCPServer("Multi-SSH-Commander", "1.0.0", server.WithLogger(slogger))

	// 3.1 自定义工具调用日志中间件：打印 tool 名、参数、耗时、失败原因
	//     覆盖框架自带日志不记录 parameters 的缺口
	mcpServer.Use(func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// DEBUG 模式：打印客户端发送的原始请求内容（含全部参数，可能含敏感信息，仅调试用）
			if slogger.Enabled(ctx, slog.LevelDebug) {
				if raw, mErr := json.Marshal(request); mErr == nil {
					slogger.Debug("mcp.tool.raw_request",
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
			lg := slogger.With(
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
	})

	// 4. 注册工具

	// 工具 1: 列出所有可用服务器
	toolOpts := []mcp.ToolOption{
		mcp.WithDescription("列出所有已配置的远程服务器及其连接信息"),
		mcp.WithReadOnlyHintAnnotation(true), // 只读：仅列举，不改动远程
	}
	if !cfg.ToolValidation.AllowAdditionalProperties {
		toolOpts = append(toolOpts, mcp.WithSchemaAdditionalProperties(false))
	}
	mcpServer.AddTool(mcp.NewTool("list_servers", toolOpts...), handleListServers(sm))

	// 工具 2: 在指定服务器上执行命令
	toolOpts = []mcp.ToolOption{
		mcp.WithDescription("在指定的远程服务器上执行 Shell 命令并返回输出结果"),
		mcp.WithDestructiveHintAnnotation(true), // 会改动远程环境（执行任意命令）
		mcp.WithOpenWorldHintAnnotation(true),   // 命令内容由调用方决定，作用域开放
		mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称（从 list_servers 获取）")),
		mcp.WithString("command", mcp.Required(), mcp.Description("要执行的 Shell 命令")),
		mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
	}
	if !cfg.ToolValidation.AllowAdditionalProperties {
		toolOpts = append(toolOpts, mcp.WithSchemaAdditionalProperties(false))
	}
	mcpServer.AddTool(mcp.NewTool("execute_command", toolOpts...), handleExecuteCommand(sm))

	// 工具 3: 读取远程文件
	toolOpts = []mcp.ToolOption{
		mcp.WithDescription("通过 SFTP 子系统从指定的远程服务器读取文件内容。提供 offset_lines/limit_lines 即进入行号分页（按行读取，无需算字节偏移）；否则走 offset/limit 字节分页"),
		mcp.WithReadOnlyHintAnnotation(true), // 只读：不改动远程文件
		mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称")),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件的绝对路径")),
		mcp.WithNumber("offset", mcp.Description("起始字节偏移（默认 0，从头开始）；与行号分页互斥")),
		mcp.WithNumber("limit", mcp.Description("读取的最大字节数（默认 0，读到文件末尾）；与行号分页互斥")),
		mcp.WithNumber("offset_lines", mcp.Description("起始行号（1-based）；提供 >0 即进入行号分页，否则走字节分页")),
		mcp.WithNumber("limit_lines", mcp.Description("读取的最大行数（默认 0 读到末尾）；提供 >0 即进入行号分页")),
		mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
	}
	if !cfg.ToolValidation.AllowAdditionalProperties {
		toolOpts = append(toolOpts, mcp.WithSchemaAdditionalProperties(false))
	}
	mcpServer.AddTool(mcp.NewTool("read_file", toolOpts...), handleReadFile(sm))

	// 工具 4: 写入远程文件
	toolOpts = []mcp.ToolOption{
		mcp.WithDescription("向指定的远程服务器写入文件内容（SFTP 子系统，流式分块写，无 base64 膨胀、无单命令长度上限）"),
		mcp.WithDestructiveHintAnnotation(true), // 会覆盖/追加远程文件
		mcp.WithOpenWorldHintAnnotation(true),   // 写入路径/内容由调用方决定
		mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称")),
		mcp.WithString("path", mcp.Required(), mcp.Description("目标文件的绝对路径")),
		mcp.WithString("content", mcp.Required(), mcp.Description("要写入的完整内容")),
		mcp.WithBoolean("append", mcp.Description("追加模式：不截断原文件，从 offset 处续写（用于分块写大文件）。默认 false=覆盖")),
		mcp.WithNumber("offset", mcp.Description("写入起始字节偏移（配合 append 使用）；为负表示追加到文件末尾。默认 0")),
		mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
	}
	if !cfg.ToolValidation.AllowAdditionalProperties {
		toolOpts = append(toolOpts, mcp.WithSchemaAdditionalProperties(false))
	}
	mcpServer.AddTool(mcp.NewTool("write_file", toolOpts...), handleWriteFile(sm))

	// 工具 4b: 分块上传助手
	toolOpts = []mcp.ToolOption{
		mcp.WithDescription("分块上传助手：传入完整 content，工具内部自动按 chunk_size 切片并以 SFTP WriteAt 落盘（调用方无需计算偏移）。支持 append 模式以跨多次调用上传超过单次 body 限制（nginx 10MB）的大文件：首块 append=false 覆盖，后续 append=true 续写"),
		mcp.WithDestructiveHintAnnotation(true), // 会覆盖/追加远程文件
		mcp.WithOpenWorldHintAnnotation(true),   // 写入路径/内容由调用方决定
		mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称")),
		mcp.WithString("path", mcp.Required(), mcp.Description("目标文件的绝对路径")),
		mcp.WithString("content", mcp.Required(), mcp.Description("要写入的内容（单次建议 <=8MB；更大请拆多块）")),
		mcp.WithBoolean("append", mcp.Description("追加模式：true 追加到文件末尾（用于多块续写）。默认 false=覆盖")),
		mcp.WithNumber("chunk_size", mcp.Description("内部每次 WriteAt 的最大字节数（默认 4MB），仅影响内存/单次写大小，不改变写入结果")),
		mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
	}
	if !cfg.ToolValidation.AllowAdditionalProperties {
		toolOpts = append(toolOpts, mcp.WithSchemaAdditionalProperties(false))
	}
	mcpServer.AddTool(mcp.NewTool("write_file_chunked", toolOpts...), handleWriteFileChunked(sm))

	// 工具 5: 读取本地文件（用于查看配置、脚本等）
	toolOpts = []mcp.ToolOption{
		mcp.WithDescription("读取本地机器上的文件内容（用于查看配置文件、脚本等本地资源）"),
		mcp.WithReadOnlyHintAnnotation(true), // 只读：仅读取本地文件（且受 local_read.allowed_dirs 限制）
		mcp.WithString("path", mcp.Required(), mcp.Description("本地文件的绝对路径")),
		mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
	}
	if !cfg.ToolValidation.AllowAdditionalProperties {
		toolOpts = append(toolOpts, mcp.WithSchemaAdditionalProperties(false))
	}
	mcpServer.AddTool(mcp.NewTool("read_local_file", toolOpts...), handleReadConfigFile(sm))

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

	// 构建 Streamable HTTP 服务：端点路径 + 库内置 CORS（替代手写中间件，减少维护面）。
	streamOpts := []server.StreamableHTTPOption{
		server.WithEndpointPath(path),
	}
	if corsOpts := buildCORSOptions(httpCfg); corsOpts != nil {
		streamOpts = append(streamOpts, server.WithStreamableHTTPCORS(corsOpts...))
	}
	streamableSrv := server.NewStreamableHTTPServer(mcpServer, streamOpts...)

	// 鉴权中间件：mcp-go 库内置鉴权为 OAuth 2.1（需授权服务器），与本项目“静态 access_token 列表”模型不符，
	// 故保留轻量静态令牌中间件（Bearer / X-Access-Token）。未启用则直接透传。
	var handler http.Handler = streamableSrv
	if httpCfg.Auth.Enabled {
		handler = withAuth(handler, httpCfg)
	}

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
		log.Fatalf("HTTP MCP Server 启动失败: %v", err)
	}
}

// buildCORSOptions 将 HTTPConfig 映射为 mcp-go 库内置的 CORS 选项。
// 仅在配置了 cors_origins 时返回非 nil（即启用 CORS）；未配置则 nil（不启用 CORS，由反代/同域处理跨域）。
func buildCORSOptions(cfg HTTPConfig) []server.CORSOption {
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
func withAuth(next http.Handler, cfg HTTPConfig) http.Handler {
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

		// 鉴权失败：返回 401。CORS 由 mcp-go 库内置（作用在 streamableSrv 内层），
		// 而本 401 在到达 streamableSrv 之前产生，故此处手动补 CORS 头，保证跨域浏览器能读取该响应。
		if ao := resolveCORSOrigin(r.Header.Get("Origin"), cfg.CORSOrigins); ao != "" {
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Origin", ao)
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

// resolveCORSOrigin 根据配置解析允许访问的源（用于 auth 层 401 响应补 CORS 头）。
// 返回空字符串表示来源不被允许。库内置 CORS 已处理正常请求，此函数仅服务于 401 边缘场景。
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
