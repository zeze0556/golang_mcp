package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/pkg/sftp"
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
}

// ==================== SSH 客户端逻辑 ====================

// NewServerManager 从配置创建服务器管理器
func NewServerManager(cfg Config) *ServerManager {
	return &ServerManager{
		configs: cfg.Servers,
		clients: make(map[string]*SSHClient),
	}
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
		// 生产环境建议使用 ssh.HostKeyCallback(func(hostname string, remote string, key ssh.PublicKey) error { ... })
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
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

// RunCommand 在远程服务器上执行命令（复用持久连接）。
// 若命令因传输层断开而失败，会重连一次并重试；仅当连接确已死亡才重试，
// 命令自身非零退出不会触发，避免对任意命令做 at-least-once 的重复执行。
// 注意：若命令已在远端执行、但响应在传输途中丢失，重试可能造成重复执行——
// 这是不可靠传输上 at-least-once 重试的固有边界，对当前幂等类工具（stat/tail/base64 -d）安全。
func (sc *SSHClient) RunCommand(cmd string) (string, error) {
	conn, err := sc.getConn()
	if err != nil {
		return "", err
	}

	output, err := sc.runOnConn(conn, cmd)
	if err != nil {
		// 传输层失败判定：连接已死才重连重试
		if !sc.connIsAlive(conn) {
			sc.dropConn(conn)
			if conn2, derr := sc.getConn(); derr == nil {
				if out2, rerr := sc.runOnConn(conn2, cmd); rerr == nil {
					return out2, nil
				}
			}
		}
		return "", err
	}
	return output, nil
}

// runOnConn 在给定连接上开会话执行命令，返回 stdout；非零退出时把 stderr 并入错误。
func (sc *SSHClient) runOnConn(conn *ssh.Client, cmd string) (string, error) {
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

		// 回执给出续写所需的偏移提示（append 时即当前文件总大小）
		nextOffset := int64(written)
		if appendMode {
			if fi, e := scli.Stat(path); e == nil {
				nextOffset = fi.Size()
			}
		}
		return mcp.NewToolResultText(fmt.Sprintf("成功分块写入文件\n服务器: %s\n路径: %s\n模式: %s\n本次写入: %d 字节\n下次续写偏移(append): %d",
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

// handleReadRemoteFile 通过工具调用读取本地配置文件中的文件（资源功能）
func handleReadConfigFile(sm *ServerManager) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := req.GetString("path", "")
		if path == "" {
			return mcp.NewToolResultError("参数 'path' 是必填项"), nil
		}

		data, err := os.ReadFile(path)
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
		mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
	), handleExecuteCommand(sm))

	// 工具 3: 读取远程文件
	mcpServer.AddTool(mcp.NewTool("read_file",
		mcp.WithDescription("通过 SFTP 子系统从指定的远程服务器读取文件内容。提供 offset_lines/limit_lines 即进入行号分页（按行读取，无需算字节偏移）；否则走 offset/limit 字节分页"),
		mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称")),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件的绝对路径")),
		mcp.WithNumber("offset", mcp.Description("起始字节偏移（默认 0，从头开始）；与行号分页互斥")),
		mcp.WithNumber("limit", mcp.Description("读取的最大字节数（默认 0，读到文件末尾）；与行号分页互斥")),
		mcp.WithNumber("offset_lines", mcp.Description("起始行号（1-based，默认 1 从头）；提供即进入行号分页")),
		mcp.WithNumber("limit_lines", mcp.Description("读取的最大行数（默认 0 读到末尾）；提供即进入行号分页")),
		mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
	), handleReadFile(sm))

	// 工具 4: 写入远程文件
	mcpServer.AddTool(mcp.NewTool("write_file",
		mcp.WithDescription("向指定的远程服务器写入文件内容（SFTP 子系统，流式分块写，无 base64 膨胀、无单命令长度上限）"),
		mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称")),
		mcp.WithString("path", mcp.Required(), mcp.Description("目标文件的绝对路径")),
		mcp.WithString("content", mcp.Required(), mcp.Description("要写入的完整内容")),
		mcp.WithBoolean("append", mcp.Description("追加模式：不截断原文件，从 offset 处续写（用于分块写大文件）。默认 false=覆盖")),
		mcp.WithNumber("offset", mcp.Description("写入起始字节偏移（配合 append 使用）；为负表示追加到文件末尾。默认 0")),
		mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
	), handleWriteFile(sm))

	// 工具 4b: 分块上传助手
	mcpServer.AddTool(mcp.NewTool("write_file_chunked",
		mcp.WithDescription("分块上传助手：传入完整 content，工具内部自动按 chunk_size 切片并以 SFTP WriteAt 落盘（调用方无需计算偏移）。支持 append 模式以跨多次调用上传超过单次 body 限制（nginx 10MB）的大文件：首块 append=false 覆盖，后续 append=true 续写"),
		mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称")),
		mcp.WithString("path", mcp.Required(), mcp.Description("目标文件的绝对路径")),
		mcp.WithString("content", mcp.Required(), mcp.Description("要写入的内容（单次建议 <=8MB；更大请拆多块）")),
		mcp.WithBoolean("append", mcp.Description("追加模式：true 追加到文件末尾（用于多块续写）。默认 false=覆盖")),
		mcp.WithNumber("chunk_size", mcp.Description("内部每次 WriteAt 的最大字节数（默认 4MB），仅影响内存/单次写大小，不改变写入结果")),
		mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
	), handleWriteFileChunked(sm))

	// 工具 5: 读取本地文件（用于查看配置、脚本等）
	mcpServer.AddTool(mcp.NewTool("read_local_file",
		mcp.WithDescription("读取本地机器上的文件内容（用于查看配置文件、脚本等本地资源）"),
		mcp.WithString("path", mcp.Required(), mcp.Description("本地文件的绝对路径")),
		mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
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
	streamableSrv := server.NewStreamableHTTPServer(mcpServer, server.WithEndpointPath(path))

	// 鉴权中间件：校验 access_token，防止端点被未授权访问（仅 HTTP 模式生效）
	authHandler := withAuth(streamableSrv, httpCfg.Auth)
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
