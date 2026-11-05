package sshmcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SSHClient 封装 SSH 连接（连接池：复用单条长连接，避免每次调用都重建 SSH 握手）
type SSHClient struct {
	mu         sync.Mutex
	config     *ssh.ClientConfig
	addr       string
	client     *ssh.Client  // 持久 SSH 连接，懒建立、可重连；nil 表示需重建
	sftpClient *sftp.Client // 与 client 绑定的 SFTP 子系统句柄，随连接复用；nil 表示需重建
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
