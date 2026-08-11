package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"gopit/internal/client/agent"
	clientmanagement "gopit/internal/client/management"
	"gopit/internal/client/manager"
	clientui "gopit/internal/client/tui"
	"gopit/internal/config"
	"gopit/internal/observability"
	"gopit/internal/runtime"
	"gopit/internal/server/app"
	"gopit/internal/server/configstore"
	"gopit/internal/server/management"
	serverui "gopit/internal/server/tui"
	"gopit/internal/tunnel"
)

const usage = `GoPit - 隧道代理工具

用法:
	pit <command> [flags]

命令:
	start                             进入并启动服务端模式
	join <token> [-s host[:port]]     进入并启动客户端模式
	tui                               打开当前模式的管理界面
	stop                              停止当前模式
	restart                           重启当前模式
	startup enable|disable            开启或关闭开机启动
	logs                              持续查看当前模式日志
	log  <name> [-d logs/]             查看指定日志文件

示例:
	pit start
	pit stop
	pit tui
	pit join my-secret-token -s 192.168.1.188
	pit logs
	pit log client-abc123

说明:
	start    选择服务端模式并写入 pit.yaml，然后后台启动。

	tui      根据 pit.yaml 读取当前模式，显示对应的服务端或客户端管理界面。

	stop     停止 pit.yaml 中当前模式的后台实例。

	restart  按 pit.yaml 重启当前模式。

	join     选择客户端模式并写入 pit.yaml，支持多个 Token 隧道。
	         -s 只传主机时自动使用服务端默认端口 7001。
	         -s 未指定时从已有配置文件读取。
	         配置自动保存到 -c 指定的路径（默认 client.yaml）。
	         使用 --foreground 可在当前终端查看彩色日志。

	startup  enable 后开机时自动按 pit.yaml 恢复当前模式。

	logs     持续查看当前模式日志。

	log      查看指定日志文件，<name> 为文件名（不含 .log 后缀）。`

const (
	commandName             = "pit"
	defaultServerConfigPath = "server.yaml"
	defaultClientConfigPath = "client.yaml"
	defaultStatePath        = "pit.yaml"
	defaultTunnelPort       = "7001"
	daemonFlag              = "--daemon"
	processStartWait        = 2 * time.Second
	processStopWait         = 5 * time.Second
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(0)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "start":
		cmdStart(args)
	case "tui":
		cmdTUI(args)
	case "stop":
		cmdStop(args)
	case "restart":
		cmdRestart(args)
	case "join":
		cmdJoin(args)
	case "startup":
		cmdStartup(args)
	case "resume":
		cmdResume()
	case "logs":
		cmdLogs(args)
	case "log":
		cmdLog(args)
	case "-h", "--help", "help":
		fmt.Println(usage)
	case "-v", "--version", "version":
		fmt.Println(commandName, version)
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		fmt.Println(usage)
		os.Exit(1)
	}
}

// --- start: 启动服务端 --------------------------------------------------

func cmdStart(args []string) {
	configPath := defaultServerConfigPath
	daemon := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c", "-config":
			if i+1 < len(args) {
				configPath = args[i+1]
				i++
			}
		case daemonFlag:
			daemon = true
		case "-h", "--help":
			fmt.Println("用法: pit start [-c server.yaml]")
			return
		}
	}
	if isClientRunning(clientConfigPathFor(configPath)) {
		fmt.Fprintln(os.Stderr, "启动失败: 客户端 join 模式正在运行，请先执行 pit stop -c client.yaml")
		os.Exit(1)
	}
	if err := saveRuntimeState(runtime.ModeServer, configPath); err != nil {
		fmt.Fprintf(os.Stderr, "保存运行模式失败: %v\n", err)
		os.Exit(1)
	}

	// 配置文件不存在时创建默认配置
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		fmt.Printf("配置文件 %s 不存在，正在创建默认配置...\n", configPath)
		if err := createDefaultServerConfig(configPath); err != nil {
			fmt.Fprintf(os.Stderr, "创建默认配置失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("默认配置已创建: %s\n", configPath)
	}

	if !daemon {
		startDaemon(configPath)
		return
	}
	runServer(configPath, false, true)
}

// cmdTUI 在前台启动服务端和管理界面。
func cmdTUI(args []string) {
	if len(args) == 0 {
		state, err := loadRuntimeState()
		if err == nil {
			switch state.Mode {
			case runtime.ModeClient:
				if !isClientRunning(state.ConfigPath) {
					startClientDaemon(state.ConfigPath)
				}
				runClientTUI(state.ConfigPath)
				return
			case runtime.ModeServer:
				if isServerRunning(state.ConfigPath) {
					runAttachedTUI(state.ConfigPath)
				} else {
					runServer(state.ConfigPath, true, false)
				}
				return
			}
		}
	}
	configPath := parseConfigPath(args)
	if isClientConfig(configPath) && isClientRunning(configPath) {
		runClientTUI(configPath)
		return
	}
	if isServerRunning(configPath) {
		runAttachedTUI(configPath)
		return
	}
	clientPath := clientConfigPathFor(configPath)
	if isClientRunning(clientPath) {
		runClientTUI(clientPath)
		return
	}
	ensureServerConfig(configPath)
	runServer(configPath, true, false)
}

// runAttachedTUI 打开已运行后台服务的配置管理界面，不重复绑定服务端端口。
func runAttachedTUI(configPath string) {
	logger := observability.NewLogger(slog.LevelInfo)
	store, err := configstore.NewStore(configPath, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	socketPath := management.SocketPath(configPath)
	model := serverui.NewAttached(store, logger, func() (app.Stats, []app.OnlineClientInfo, error) {
		snapshot, err := management.ReadSnapshot(socketPath)
		return snapshot.Stats, snapshot.OnlineClients, err
	})
	model.SetModeSwitcher(func() error { return saveRuntimeState(runtime.ModeClient, clientConfigPathFor(configPath)) })
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI 错误: %v\n", err)
	}
}

// runClientTUI 打开客户端后台管理界面，不会重复建立隧道连接。
func runClientTUI(configPath string) {
	logger := observability.NewLogger(slog.LevelInfo)
	model := clientui.NewAttached(configPath, logger)
	model.SetModeSwitcher(func() error { return saveRuntimeState(runtime.ModeServer, serverConfigPathFor(configPath)) })
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "客户端 TUI 错误: %v\n", err)
	}
}

// startDaemon 将服务端进程脱离当前终端并返回。
func startDaemon(configPath string) {
	if err := ensureNotRunning(configPath); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "定位可执行文件失败: %v\n", err)
		os.Exit(1)
	}
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析配置路径失败: %v\n", err)
		os.Exit(1)
	}
	logPath := strings.TrimSuffix(absConfig, filepath.Ext(absConfig)) + ".log"
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开日志失败: %v\n", err)
		os.Exit(1)
	}
	cmd := exec.Command(executable, "start", "-c", absConfig, daemonFlag)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		fmt.Fprintf(os.Stderr, "启动后台服务失败: %v\n", err)
		os.Exit(1)
	}
	logFile.Close()
	pid := cmd.Process.Pid
	cmd.Process.Release()
	deadline := time.Now().Add(processStartWait)
	for time.Now().Before(deadline) {
		if runningPID, err := readPID(absConfig); err == nil && runningPID == pid {
			fmt.Printf("服务端已启动，PID %d\n", pid)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "后台服务启动失败，请查看日志: %s\n", logPath)
	os.Exit(1)
}

// runServer 启动服务端并按模式等待 TUI 或终止信号。
func runServer(configPath string, showTUI, daemon bool) {
	var logger *slog.Logger
	var closeLog func()
	if daemon {
		logPath := strings.TrimSuffix(configPath, filepath.Ext(configPath)) + ".log"
		var err error
		var file *os.File
		logger, file, err = observability.NewFileLogger(logPath, slog.LevelInfo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "打开服务端日志失败: %v\n", err)
			os.Exit(1)
		}
		closeLog = func() { file.Close() }
	} else {
		logger = observability.NewLogger(slog.LevelInfo)
		closeLog = func() {}
	}
	defer closeLog()
	store, err := configstore.NewStore(configPath, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	application := app.New(store, logger, tunnel.DefaultConfig())
	if err := application.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "启动服务端失败: %v\n", err)
		os.Exit(1)
	}
	socketPath := management.SocketPath(configPath)
	statusListener, err := management.StartStatusServer(socketPath, application, logger)
	if err != nil {
		application.Stop()
		fmt.Fprintf(os.Stderr, "启动状态服务失败: %v\n", err)
		os.Exit(1)
	}
	defer statusListener.Close()
	defer os.Remove(socketPath)
	pidFile := pidFilePath(configPath)
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0600); err != nil {
		logger.Warn("write pid file failed", "err", err)
	}
	defer os.Remove(pidFile)
	defer application.Stop()
	if showTUI {
		model := serverui.New(application, store, logger)
		p := tea.NewProgram(model, tea.WithAltScreen())
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "TUI 错误: %v\n", err)
		}
		return
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
}

// runClient 启动多隧道客户端后台管理器。
func runClient(configPath string, daemon bool) {
	var logger *slog.Logger
	var closeLog func()
	if daemon {
		logPath := strings.TrimSuffix(configPath, filepath.Ext(configPath)) + ".log"
		fileLogger, file, err := observability.NewFileLogger(logPath, slog.LevelInfo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "打开客户端日志失败: %v\n", err)
			os.Exit(1)
		}
		logger, closeLog = fileLogger, func() { file.Close() }
	} else {
		logger, closeLog = observability.NewColorLogger(slog.LevelInfo), func() {}
	}
	defer closeLog()
	runtime := manager.New(configPath, logger, tunnel.DefaultConfig())
	if err := runtime.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "启动客户端失败: %v\n", err)
		os.Exit(1)
	}
	defer runtime.Stop()
	socketPath := clientmanagement.SocketPath(configPath)
	listener, err := clientmanagement.StartStatusServer(socketPath, runtime, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动客户端状态服务失败: %v\n", err)
		os.Exit(1)
	}
	defer listener.Close()
	defer os.Remove(socketPath)
	pidPath := pidFilePath(configPath)
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0600); err != nil {
		logger.Warn("write client pid file failed", "err", err)
	}
	defer os.Remove(pidPath)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
}

func parseConfigPath(args []string) string {
	configPath := defaultServerConfigPath
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" || args[i] == "-config" {
			if i+1 < len(args) {
				configPath, i = args[i+1], i+1
			}
		}
	}
	return configPath
}

// hasHelpFlag 判断命令参数是否请求帮助。
func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func ensureServerConfig(configPath string) {
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := createDefaultServerConfig(configPath); err != nil {
			fmt.Fprintf(os.Stderr, "创建默认配置失败: %v\n", err)
			os.Exit(1)
		}
	}
}

func ensureNotRunning(configPath string) error {
	if isServerRunning(configPath) {
		pid, _ := readPID(configPath)
		return fmt.Errorf("服务端已运行 (PID %d)", pid)
	}
	return nil
}

func isClientRunning(configPath string) bool { return isProcessRunning(configPath) }

// isClientConfig 通过持久化的多隧道字段判断配置是否属于 join 模式。
func isClientConfig(configPath string) bool {
	cfg, err := config.LoadClientConfig(configPath)
	return err == nil && (len(cfg.Tunnels) > 0 || (cfg.Server.Address != "" && cfg.Auth.Token != ""))
}

// isServerRunning 判断 PID 文件指向的服务端进程是否仍在运行。
func isServerRunning(configPath string) bool {
	return isProcessRunning(configPath)
}

func isProcessRunning(configPath string) bool {
	pid, err := readPID(configPath)
	if err != nil {
		return false
	}
	proc, err := os.FindProcess(pid)
	return err == nil && proc.Signal(syscall.Signal(0)) == nil
}

// readPID 读取并解析指定配置对应的服务端进程号。
func readPID(configPath string) (int, error) {
	data, err := os.ReadFile(pidFilePath(configPath))
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return 0, fmt.Errorf("解析 PID 失败: %w", err)
	}
	return pid, nil
}

// --- stop: 停止服务端 ---------------------------------------------------

func cmdStop(args []string) {
	if hasHelpFlag(args) {
		fmt.Println("用法: pit stop [-c server.yaml]")
		return
	}
	configPath := parseConfigPath(args)
	if len(args) == 0 {
		if state, err := loadRuntimeState(); err == nil {
			configPath = state.ConfigPath
		}
	}
	if !isProcessRunning(configPath) && configPath == defaultServerConfigPath && isClientRunning(defaultClientConfigPath) {
		configPath = defaultClientConfigPath
	}

	if err := stopByPID(configPath); err != nil {
		fmt.Fprintf(os.Stderr, "停止失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("后台实例已停止")
}

// --- restart: 重启服务端 -------------------------------------------------

func cmdRestart(args []string) {
	if hasHelpFlag(args) {
		fmt.Println("用法: pit restart [-c server.yaml]")
		return
	}
	configPath := parseConfigPath(args)
	if len(args) == 0 {
		if state, err := loadRuntimeState(); err == nil {
			if err := stopByPID(state.ConfigPath); err == nil {
				fmt.Println("已停止旧实例")
			}
			if state.Mode == runtime.ModeServer {
				// TUI 切换模式后，状态文件指向新模式；同时清理可能仍在运行的客户端。
				_ = stopByPID(clientConfigPathFor(state.ConfigPath))
				cmdStart([]string{"-c", state.ConfigPath})
			} else {
				// 客户端模式同理，先停止同目录下的服务端实例。
				_ = stopByPID(serverConfigPathFor(state.ConfigPath))
				startClientDaemon(state.ConfigPath)
			}
			return
		}
	}

	// 先尝试停止运行中的实例（忽略错误，可能未运行）
	if err := stopByPID(configPath); err == nil {
		fmt.Println("已停止旧实例")
	} else {
		fmt.Println("无运行中的实例，直接启动...")
	}

	// 重新启动
	cmdStart(args)
}

// --- PID 文件管理 -------------------------------------------------------

// pidFilePath 根据配置文件路径生成 PID 文件路径。
// 例如 server.yaml -> server.pid
func pidFilePath(configPath string) string {
	ext := filepath.Ext(configPath)
	base := strings.TrimSuffix(configPath, ext)
	return base + ".pid"
}

// stopByPID 读取 PID 文件并向对应进程发送 SIGTERM。
func stopByPID(configPath string) error {
	pidFile := pidFilePath(configPath)
	pid, err := readPID(configPath)
	if err != nil {
		return fmt.Errorf("读取 PID 文件失败（服务端未运行?）: %w", err)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("查找进程失败: %w", err)
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			os.Remove(pidFile)
			return nil
		}
		return fmt.Errorf("发送信号失败: %w", err)
	}

	deadline := time.Now().Add(processStopWait)
	for time.Now().Before(deadline) {
		if proc.Signal(syscall.Signal(0)) != nil {
			os.Remove(pidFile)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("等待进程 %d 退出超时", pid)
}

// --- join: 以 Token 连接，输出日志 ---------------------------------------

func cmdJoin(args []string) {
	if hasHelpFlag(args) {
		fmt.Println("用法: pit join <token> [-s host[:port]] [-c client.yaml] [--foreground]\n       pit join --run [-c client.yaml]")
		return
	}
	token := ""
	serverAddr := ""
	configPath := defaultClientConfigPath
	foreground := false
	runOnly := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-s", "-server":
			if i+1 < len(args) {
				serverAddr = args[i+1]
				i++
			}
		case "-c", "-config":
			if i+1 < len(args) {
				configPath = args[i+1]
				i++
			}
		case "--foreground", "-f":
			foreground = true
		case "--run":
			runOnly = true
		default:
			if !strings.HasPrefix(args[i], "-") && token == "" {
				token = args[i]
			}
		}
	}
	if isServerRunning(serverConfigPathFor(configPath)) {
		fmt.Fprintln(os.Stderr, "加入失败: 服务端 start 模式正在运行，请先执行 pit stop -c server.yaml")
		os.Exit(1)
	}
	if isClientRunning(configPath) && !foreground {
		pid, _ := readPID(configPath)
		fmt.Fprintf(os.Stderr, "客户端已运行 (PID %d)，使用 pit tui -c %s 管理\n", pid, configPath)
		os.Exit(1)
	}
	if runOnly {
		if _, err := config.LoadClientConfig(configPath); err != nil {
			fmt.Fprintf(os.Stderr, "加载客户端配置失败: %v\n", err)
			os.Exit(1)
		}
		// --run 仅由已脱离终端的子进程及 LaunchAgent 使用，直接运行管理器。
		runClient(configPath, true)
		return
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "用法: pit join <token> [-s host[:port]] [-c client.yaml]")
		os.Exit(1)
	}

	// 如果未指定 server 地址，尝试从已有配置读取
	if serverAddr == "" {
		if existing, err := config.LoadClientConfig(configPath); err == nil && existing != nil {
			serverAddr = existing.Server.Address
		}
	}
	if serverAddr == "" {
		fmt.Fprintf(os.Stderr, "错误: 未指定服务端地址，请使用 -s 参数\n")
		os.Exit(1)
	}
	serverAddr, err := normalizeServerAddress(serverAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 服务端地址无效: %v\n", err)
		os.Exit(1)
	}

	if err := addClientTunnel(configPath, serverAddr, token); err != nil {
		fmt.Fprintf(os.Stderr, "保存配置失败: %v\n", err)
		os.Exit(1)
	}
	if err := saveRuntimeState(runtime.ModeClient, configPath); err != nil {
		fmt.Fprintf(os.Stderr, "保存运行模式失败: %v\n", err)
		os.Exit(1)
	}

	if !foreground {
		startClientDaemon(configPath)
		return
	}
	// 前台模式保留单隧道彩色日志，便于临时诊断。
	logger := observability.NewColorLogger(slog.LevelInfo)

	fmt.Printf("连接到 %s ...\n", serverAddr)

	// 创建并启动 Agent
	cfg := config.ClientConfig{
		Server: config.ServerRef{Address: serverAddr},
		Auth:   config.AuthRef{Token: token},
	}

	tunnelCfg := tunnel.DefaultConfig()
	a := agent.New(cfg, configPath, logger, tunnelCfg)

	// 设置状态回调，输出状态变更
	a.OnStatusChange = func(info agent.StatusInfo) {
		switch info.Status {
		case "connecting":
			logger.Info("正在连接...", "server", info.RemoteAddr)
		case "connected":
			logger.Info("已连接", "conn_id", info.ConnectionID, "target", info.Target)
		case "auth_failed":
			logger.Error("鉴权失败", "err", info.Error)
		case "disconnected":
			if info.Error != "" {
				logger.Warn("连接断开", "err", info.Error)
			} else {
				logger.Info("连接断开，准备重连...")
			}
		}
	}

	a.Start()
	defer a.Stop()

	// 等待 Ctrl+C
	fmt.Println("按 Ctrl+C 断开连接")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	fmt.Println("\n正在断开连接...")
}

// startClientDaemon 将客户端管理器脱离终端运行。
func startClientDaemon(configPath string) {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "定位可执行文件失败: %v\n", err)
		os.Exit(1)
	}
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析配置路径失败: %v\n", err)
		os.Exit(1)
	}
	logPath := strings.TrimSuffix(absConfig, filepath.Ext(absConfig)) + ".log"
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开日志失败: %v\n", err)
		os.Exit(1)
	}
	cmd := exec.Command(executable, "join", "--run", "-c", absConfig)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = logFile, logFile, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		fmt.Fprintf(os.Stderr, "启动后台客户端失败: %v\n", err)
		os.Exit(1)
	}
	logFile.Close()
	pid := cmd.Process.Pid
	cmd.Process.Release()
	deadline := time.Now().Add(processStartWait)
	for time.Now().Before(deadline) {
		if running, err := readPID(absConfig); err == nil && running == pid {
			fmt.Printf("客户端已启动，PID %d\n", pid)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "后台客户端启动失败，请查看日志: %s\n", logPath)
	os.Exit(1)
}

func addClientTunnel(path, serverAddr, token string) error {
	cfg, err := config.LoadClientConfig(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if cfg == nil {
		cfg = &config.ClientConfig{}
	}
	items := cfg.Tunnels
	if len(items) == 0 && cfg.Server.Address != "" && cfg.Auth.Token != "" {
		items = append(items, config.ClientTunnel{ID: "default", Name: "default", Server: cfg.Server.Address, Token: cfg.Auth.Token, Enabled: true})
	}
	for _, item := range items {
		if item.Server == serverAddr && item.Token == token {
			return fmt.Errorf("该隧道已存在")
		}
	}
	id, err := newClientTunnelID()
	if err != nil {
		return err
	}
	items = append(items, config.ClientTunnel{ID: id, Name: id, Server: serverAddr, Token: token, Enabled: true, CreatedAt: time.Now().Format(time.RFC3339)})
	cfg.Server, cfg.Auth, cfg.Tunnels = config.ServerRef{}, config.AuthRef{}, items
	return config.SaveClientConfig(path, cfg)
}

func newClientTunnelID() (string, error) {
	data := make([]byte, 6)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "client-" + hex.EncodeToString(data), nil
}

func clientConfigPathFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), defaultClientConfigPath)
}
func serverConfigPathFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), defaultServerConfigPath)
}

// normalizeServerAddress 为未指定端口的服务端主机补充默认隧道端口。
func normalizeServerAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", fmt.Errorf("地址不能为空")
	}
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address, nil
	}
	if net.ParseIP(address) != nil || !strings.Contains(address, ":") {
		return net.JoinHostPort(address, defaultTunnelPort), nil
	}
	return "", fmt.Errorf("请使用 host、host:port 或 [ipv6]:port 格式")
}

// cmdStartup 安装或移除 macOS LaunchAgent；具体模式由 pit.yaml 决定。
func cmdStartup(args []string) {
	if len(args) == 0 || hasHelpFlag(args) {
		fmt.Println("用法: pit startup enable|disable")
		return
	}
	if args[0] == "disable" {
		if err := removeStartup(); err != nil {
			fmt.Fprintf(os.Stderr, "移除开机启动失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("已移除开机启动")
		return
	}
	if args[0] != "enable" {
		fmt.Fprintln(os.Stderr, "用法: pit startup enable|disable")
		os.Exit(1)
	}
	if _, err := loadRuntimeState(); err != nil {
		fmt.Fprintf(os.Stderr, "开启失败: 请先执行 pit start 或 pit join 保存运行模式: %v\n", err)
		os.Exit(1)
	}
	if err := installStartup(); err != nil {
		fmt.Fprintf(os.Stderr, "安装开机启动失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("已开启开机启动")
}

const startupLabel = "com.gopit"

func startupPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", startupLabel+".plist"), nil
}

func installStartup() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	path, err := startupPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	args := []string{"resume"}
	entries := make([]string, 0, len(args)+1)
	entries = append(entries, plistString(executable))
	for _, arg := range args {
		entries = append(entries, plistString(arg))
	}
	logPath := "pit.log"
	content := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n<key>Label</key><string>" + startupLabel + "</string>\n<key>WorkingDirectory</key>" + plistString(mustCurrentDirectory()) + "\n<key>ProgramArguments</key><array>" + strings.Join(entries, "") + "</array>\n<key>RunAtLoad</key><true/>\n<key>StandardOutPath</key>" + plistString(logPath) + "\n<key>StandardErrorPath</key>" + plistString(logPath) + "\n</dict></plist>\n"
	return os.WriteFile(path, []byte(content), 0600)
}

func removeStartup() error {
	path, err := startupPlistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// cmdResume 仅由开机启动项调用，根据持久化状态恢复上次选择的模式。
func cmdResume() {
	state, err := loadRuntimeState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "恢复失败: %v\n", err)
		os.Exit(1)
	}
	switch state.Mode {
	case runtime.ModeServer:
		runServer(state.ConfigPath, false, true)
	case runtime.ModeClient:
		runClient(state.ConfigPath, true)
	}
}

// saveRuntimeState 将当前模式写入统一状态文件，供 tui、restart 与开机启动读取。
func saveRuntimeState(mode, configPath string) error {
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	return runtime.Save(defaultStatePath, runtime.State{Mode: mode, ConfigPath: absPath})
}

func loadRuntimeState() (*runtime.State, error) { return runtime.Load(defaultStatePath) }

func mustCurrentDirectory() string {
	directory, err := os.Getwd()
	if err != nil {
		return "."
	}
	return directory
}

func plistString(value string) string {
	return "<string>" + strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;").Replace(value) + "</string>"
}

// --- logs: 查看服务端日志 ------------------------------------------------

func cmdLogs(args []string) {
	if hasHelpFlag(args) {
		fmt.Println("用法: pit logs [-c server.yaml]")
		return
	}
	configPath := parseConfigPath(args)
	if len(args) == 0 {
		if state, err := loadRuntimeState(); err == nil {
			configPath = state.ConfigPath
		}
	}
	logPath := strings.TrimSuffix(configPath, filepath.Ext(configPath)) + ".log"
	fmt.Printf("查看日志: %s (Ctrl+C 退出)\n\n", logPath)
	tailFile(logPath)
}

// --- log: 查看指定日志文件 ---------------------------------------------

func cmdLog(args []string) {
	if len(args) == 0 {
		fmt.Println("用法: pit log <name> [-d logs/]")
		os.Exit(1)
	}

	name := args[0]
	logDir := "logs"
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-d", "-dir":
			if i+1 < len(args) {
				logDir = args[i+1]
				i++
			}
		case "-h", "--help":
			fmt.Println("用法: pit log <name> [-d logs/]")
			return
		}
	}

	path := filepath.Join(logDir, name+".log")
	if _, err := os.Stat(path); err != nil {
		if _, err2 := os.Stat(name); err2 == nil {
			path = name
		} else {
			fmt.Fprintf(os.Stderr, "日志文件不存在: %s\n", path)
			os.Exit(1)
		}
	}

	fmt.Printf("查看日志: %s (Ctrl+C 退出)\n\n", path)
	tailFile(path)
}

// --- helpers ------------------------------------------------------------

func createDefaultServerConfig(path string) error {
	cfg := &config.ServerConfig{
		Server: config.ServerSection{
			Host:          "0.0.0.0",
			Port:          7001,
			TunnelListen:  ":7001",
			HTTPListen:    ":80",
			HTTPSListen:   ":443",
			ConfigVersion: 1,
		},
		TLS: config.TLSSection{
			Enabled: false,
		},
		Connections: []config.Connection{},
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}

	return config.SaveServerConfig(path, cfg)
}
