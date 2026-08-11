package app

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"gopit/internal/config"
	"gopit/internal/server/auth"
	"gopit/internal/server/configstore"
	"gopit/internal/tunnel"
)

// TestFindHTTPConnection 验证默认 Web 端口仅按 Host 精确定位 HTTP 隧道。
func TestFindHTTPConnection(t *testing.T) {
	application := newHTTPTestApp(t, []config.Connection{
		newHTTPTestConnection(t, "api", "api.example.com", 0),
		newHTTPTestConnection(t, "fallback", "", 0),
	})

	rule, found := application.findHTTPConnection("API.EXAMPLE.COM:7777")
	if !found || rule.ID != "api" {
		t.Fatalf("findHTTPConnection() = (%q, %t), want (api, true)", rule.ID, found)
	}
	if _, found := application.findHTTPConnection("unknown.example.com"); found {
		t.Fatal("unknown Host unexpectedly matched an HTTP tunnel")
	}
}

// TestSyncHTTPPortListeners 验证 HTTP 独立端口变更后立即切换监听器。
func TestSyncHTTPPortListeners(t *testing.T) {
	firstPort := freeTCPPort(t)
	secondPort := freeTCPPort(t)
	connection := newHTTPTestConnection(t, "direct", "", firstPort)
	application := newHTTPTestApp(t, []config.Connection{connection})
	defer application.Stop()

	application.startHTTPPortListeners(application.store.Get())
	assertTCPPortOpen(t, firstPort)

	connection.ListenPort = secondPort
	updated := &config.ServerConfig{
		Server:      config.ServerSection{TunnelListen: ":0", HTTPListen: ":7777"},
		Connections: []config.Connection{connection},
	}
	application.syncHTTPPortListeners(updated)
	assertTCPPortClosed(t, firstPort)
	assertTCPPortOpen(t, secondPort)
}

// newHTTPTestApp 创建仅用于 HTTP 路由和监听测试的服务端实例。
func newHTTPTestApp(t *testing.T, connections []config.Connection) *App {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "server.yaml")
	cfg := &config.ServerConfig{
		Server:      config.ServerSection{TunnelListen: ":0", HTTPListen: ":7777"},
		Connections: connections,
	}
	if err := config.SaveServerConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveServerConfig() error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := configstore.NewStore(configPath, logger)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return New(store, logger, tunnel.DefaultConfig())
}

// newHTTPTestConnection 创建通过配置校验的 HTTP 规则。
func newHTTPTestConnection(t *testing.T, id, host string, port int) config.Connection {
	t.Helper()
	tokenHash, err := auth.HashToken("test-token-" + id)
	if err != nil {
		t.Fatalf("HashToken() error = %v", err)
	}
	return config.Connection{
		ID:         id,
		Name:       id,
		Type:       config.ConnectionTypeHTTP,
		Host:       host,
		ListenPort: port,
		Target:     "127.0.0.1:8080",
		TokenHash:  tokenHash,
		Enabled:    true,
	}
}

// freeTCPPort 获取一个当前可用的本地 TCP 端口。
func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// assertTCPPortOpen 验证指定端口已开放监听。
func assertTCPPortOpen(t *testing.T, port int) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), time.Second)
	if err != nil {
		t.Fatalf("port %d is not listening: %v", port, err)
	}
	_ = connection.Close()
}

// assertTCPPortClosed 验证指定端口已停止监听。
func assertTCPPortClosed(t *testing.T, port int) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 100*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatalf("port %d is still listening", port)
	}
}

// itoa 将测试端口转换为地址字符串。
func itoa(port int) string {
	return fmt.Sprintf("%d", port)
}
