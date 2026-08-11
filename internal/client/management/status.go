// Package management 提供客户端后台管理器和本机 TUI 的状态通信。
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

	"gopit/internal/client/manager"
)

const socketFileSuffix = ".sock"
const socketPermission = 0600
const dialTimeout = time.Second

func SocketPath(configPath string) string {
	return strings.TrimSuffix(configPath, filepath.Ext(configPath)) + socketFileSuffix
}

func StartStatusServer(path string, runtime *manager.Manager, logger *slog.Logger) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale status socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, socketPermission); err != nil {
		listener.Close()
		return nil, err
	}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				if err := json.NewEncoder(conn).Encode(runtime.GetSnapshot()); err != nil {
					logger.Debug("write client snapshot failed", "err", err)
				}
			}()
		}
	}()
	return listener, nil
}

func ReadSnapshot(path string) (manager.Snapshot, error) {
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return manager.Snapshot{}, err
	}
	defer conn.Close()
	var snapshot manager.Snapshot
	if err := json.NewDecoder(conn).Decode(&snapshot); err != nil {
		return manager.Snapshot{}, err
	}
	return snapshot, nil
}
