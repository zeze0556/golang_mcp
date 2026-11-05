package sshmcp

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// ServerManager 管理多个 SSH 连接
type ServerManager struct {
	mu      sync.RWMutex
	configs map[string]SSHConfig
	clients map[string]*SSHClient

	// cfgPath 配置文件路径；非空时支持热重载（Reload）。
	cfgPath string

	// 运行时参数（由配置解析而来，全局统一）
	cmdTimeout time.Duration // 单条命令执行超时
	maxOutput  int           // 单条命令输出采集上限（字节）
	knownHosts string        // known_hosts 文件路径
	localRead  LocalReadConfig
}

// NewServerManager 从配置创建服务器管理器。
// cfgPath 为配置文件路径（用于热重载）；独立二进制可传空字符串。
func NewServerManager(cfg Config, cfgPath string) *ServerManager {
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
		cfgPath:    cfgPath,
		cmdTimeout: cmdTimeout,
		maxOutput:  maxOutput,
		knownHosts: cfg.KnownHostsPath,
		localRead:  cfg.LocalRead,
	}
}

// ServerCount 返回当前已配置的服务器数量（并发安全，热重载后实时反映）
func (sm *ServerManager) ServerCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.configs)
}

// GetConfigPath 返回配置文件路径（用于管理端点展示）。
func (sm *ServerManager) GetConfigPath() string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.cfgPath
}

// Reload 从磁盘重新加载配置文件，热更新服务器列表（增加/删除服务器无需重启进程）。
// 已缓存的连接会被清空，下次调用时按新配置惰性重连。
// 若新配置非法（例如没有任何 servers），则保留原配置并返回错误，不会破坏现有服务。
//
// 注意：仅 servers 列表会被热更新；HTTP 鉴权令牌与 CORS 在启动时固定，
// 若需变更这两项仍需重启进程。
func (sm *ServerManager) Reload() error {
	if sm.cfgPath == "" {
		return fmt.Errorf("未记录配置文件路径，无法热重载")
	}
	newCfg, err := LoadConfig(sm.cfgPath)
	if err != nil {
		return err
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.configs = newCfg.Servers
	// 清空已缓存的连接，强制按新配置惰性重建（地址/密钥可能已变更）
	sm.clients = make(map[string]*SSHClient)
	return nil
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
	sm.mu.RLock()
	cfg, ok := sm.configs[name]
	sm.mu.RUnlock()
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
