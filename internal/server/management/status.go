// Package management 提供后台服务与本机管理 TUI 之间的状态通信。
package management

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopit/internal/server/app"
)

const (
	socketFileSuffix = ".sock"
	socketPermission = 0600
	dialTimeout      = time.Second
)

// Snapshot 是后台服务实时状态的只读快照。
type Snapshot struct {
	Stats         app.Stats              `json:"stats"`
	OnlineClients []app.OnlineClientInfo `json:"online_clients"`
}

// SocketPath 根据服务端配置文件路径返回本机管理 Socket 路径。
func SocketPath(configPath string) string {
	extension := filepath.Ext(configPath)
	base := strings.TrimSuffix(configPath, extension)
	return base + socketFileSuffix
}

// StartStatusServer 启动本机状态服务，供附加式 TUI 查询后台实时状态。
func StartStatusServer(socketPath string, application *app.App, logger *slog.Logger) (net.Listener, error) {
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale status socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen status socket: %w", err)
	}
	if err := os.Chmod(socketPath, socketPermission); err != nil {
		listener.Close()
		os.Remove(socketPath)
		return nil, fmt.Errorf("set status socket permission: %w", err)
	}
	go acceptStatusConnections(listener, application, logger)
	return listener, nil
}

// ReadSnapshot 读取后台服务通过本机 Socket 提供的实时状态。
func ReadSnapshot(socketPath string) (Snapshot, error) {
	conn, err := net.DialTimeout("unix", socketPath, dialTimeout)
	if err != nil {
		return Snapshot{}, fmt.Errorf("dial status socket: %w", err)
	}
	defer conn.Close()

	var snapshot Snapshot
	if err := json.NewDecoder(conn).Decode(&snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode status snapshot: %w", err)
	}
	return snapshot, nil
}

// acceptStatusConnections 为每个 TUI 请求返回一次状态快照。
func acceptStatusConnections(listener net.Listener, application *app.App, logger *slog.Logger) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go writeSnapshot(conn, application, logger)
	}
}

// writeSnapshot 在单个连接上写入状态并关闭连接。
func writeSnapshot(conn net.Conn, application *app.App, logger *slog.Logger) {
	defer conn.Close()
	snapshot := Snapshot{
		Stats:         application.GetStats(),
		OnlineClients: application.GetOnlineClients(),
	}
	if err := json.NewEncoder(conn).Encode(snapshot); err != nil {
		logger.Debug("write status snapshot failed", "err", err)
	}
}
