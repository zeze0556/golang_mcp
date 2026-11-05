package sshmcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/pkg/sftp"
)

// RegisterTools 向 MCP Server 注册全部工具。
// allowAdditionalProperties=false 时对所有工具启用严格模式（拒绝客户端传递未声明参数）。
func RegisterTools(s *server.MCPServer, sm *ServerManager, allowAdditionalProperties bool) {
	add := func(name string, opts []mcp.ToolOption, handler server.ToolHandlerFunc) {
		if !allowAdditionalProperties {
			opts = append(opts, mcp.WithSchemaAdditionalProperties(false))
		}
		s.AddTool(mcp.NewTool(name, opts...), handler)
	}

	// 工具 1: 列出所有可用服务器
	add("list_servers",
		[]mcp.ToolOption{
			mcp.WithDescription("列出所有已配置的远程服务器及其连接信息"),
			mcp.WithReadOnlyHintAnnotation(true), // 只读：仅列举，不改动远程
		},
		handleListServers(sm))

	// 工具 2: 在指定服务器上执行命令
	add("execute_command",
		[]mcp.ToolOption{
			mcp.WithDescription("在指定的远程服务器上执行 Shell 命令并返回输出结果"),
			mcp.WithDestructiveHintAnnotation(true), // 会改动远程环境（执行任意命令）
			mcp.WithOpenWorldHintAnnotation(true),   // 命令内容由调用方决定，作用域开放
			mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称（从 list_servers 获取）")),
			mcp.WithString("command", mcp.Required(), mcp.Description("要执行的 Shell 命令")),
			mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
		},
		handleExecuteCommand(sm))

	// 工具 3: 读取远程文件
	add("read_file",
		[]mcp.ToolOption{
			mcp.WithDescription("通过 SFTP 子系统从指定的远程服务器读取文件内容。提供 offset_lines/limit_lines 即进入行号分页（按行读取，无需算字节偏移）；否则走 offset/limit 字节分页"),
			mcp.WithReadOnlyHintAnnotation(true), // 只读：不改动远程文件
			mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称")),
			mcp.WithString("path", mcp.Required(), mcp.Description("文件的绝对路径")),
			mcp.WithNumber("offset", mcp.Description("起始字节偏移（默认 0，从头开始）；与行号分页互斥")),
			mcp.WithNumber("limit", mcp.Description("读取的最大字节数（默认 0，读到文件末尾）；与行号分页互斥")),
			mcp.WithNumber("offset_lines", mcp.Description("起始行号（1-based）；提供 >0 即进入行号分页，否则走字节分页")),
			mcp.WithNumber("limit_lines", mcp.Description("读取的最大行数（默认 0 读到末尾）；提供 >0 即进入行号分页")),
			mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
		},
		handleReadFile(sm))

	// 工具 4: 写入远程文件
	add("write_file",
		[]mcp.ToolOption{
			mcp.WithDescription("向指定的远程服务器写入文件内容（SFTP 子系统，流式分块写，无 base64 膨胀、无单命令长度上限）"),
			mcp.WithDestructiveHintAnnotation(true), // 会覆盖/追加远程文件
			mcp.WithOpenWorldHintAnnotation(true),   // 写入路径/内容由调用方决定
			mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称")),
			mcp.WithString("path", mcp.Required(), mcp.Description("目标文件的绝对路径")),
			mcp.WithString("content", mcp.Required(), mcp.Description("要写入的完整内容")),
			mcp.WithBoolean("append", mcp.Description("追加模式：不截断原文件，从 offset 处续写（用于分块写大文件）。默认 false=覆盖")),
			mcp.WithNumber("offset", mcp.Description("写入起始字节偏移（配合 append 使用）；为负表示追加到文件末尾。默认 0")),
			mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
		},
		handleWriteFile(sm))

	// 工具 4b: 分块上传助手
	add("write_file_chunked",
		[]mcp.ToolOption{
			mcp.WithDescription("分块上传助手：传入完整 content，工具内部自动按 chunk_size 切片并以 SFTP WriteAt 落盘（调用方无需计算偏移）。支持 append 模式以跨多次调用上传超过单次 body 限制（nginx 10MB）的大文件：首块 append=false 覆盖，后续 append=true 续写"),
			mcp.WithDestructiveHintAnnotation(true), // 会覆盖/追加远程文件
			mcp.WithOpenWorldHintAnnotation(true),   // 写入路径/内容由调用方决定
			mcp.WithString("server", mcp.Required(), mcp.Description("服务器名称")),
			mcp.WithString("path", mcp.Required(), mcp.Description("目标文件的绝对路径")),
			mcp.WithString("content", mcp.Required(), mcp.Description("要写入的内容（单次建议 <=8MB；更大请拆多块）")),
			mcp.WithBoolean("append", mcp.Description("追加模式：true 追加到文件末尾（用于多块续写）。默认 false=覆盖")),
			mcp.WithNumber("chunk_size", mcp.Description("内部每次 WriteAt 的最大字节数（默认 4MB），仅影响内存/单次写大小，不改变写入结果")),
			mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
		},
		handleWriteFileChunked(sm))

	// 工具 5: 读取本地文件（用于查看配置、脚本等）
	add("read_local_file",
		[]mcp.ToolOption{
			mcp.WithDescription("读取本地机器上的文件内容（用于查看配置文件、脚本等本地资源）"),
			mcp.WithReadOnlyHintAnnotation(true), // 只读：仅读取本地文件（且受 local_read.allowed_dirs 限制）
			mcp.WithString("path", mcp.Required(), mcp.Description("本地文件的绝对路径")),
			mcp.WithString("reason", mcp.Description("操作原因（审计记录）")),
		},
		handleReadConfigFile(sm))
}

// handleListServers 列出所有可用的服务器
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
			"count":   len(infos),
			"servers": infos,
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
