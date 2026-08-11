package agent

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"gopit/internal/config"
	"gopit/internal/protocol"
	"gopit/internal/tunnel"
)

// TestApplyRoutes 将服务端下发的名称和目标地址写入客户端运行状态。
func TestApplyRoutes(t *testing.T) {
	client := New(
		config.ClientConfig{Server: config.ServerRef{Address: "server.example.com:7001"}},
		"client.yaml",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		tunnel.DefaultConfig(),
	)
	client.applyRoutes([]protocol.RouteEntry{{Name: "production-api", Target: "127.0.0.1:8080"}}, 7)

	status := client.GetStatus()
	if status.Name != "production-api" {
		t.Fatalf("status.Name = %q, want production-api", status.Name)
	}
	if status.Target != "127.0.0.1:8080" {
		t.Fatalf("status.Target = %q, want 127.0.0.1:8080", status.Target)
	}
	if status.RemoteAddr != "server.example.com:7001" {
		t.Fatalf("status.RemoteAddr = %q, want server.example.com:7001", status.RemoteAddr)
	}
}

// TestStreamConnCaptureRequest 验证客户端从转发数据中提取请求日志所需字段。
func TestStreamConnCaptureRequest(t *testing.T) {
	stream := &streamConn{
		startedAt:     time.Now().Add(-20 * time.Millisecond),
		target:        "127.0.0.1:8080",
		publicAddress: "api.example.com",
	}
	stream.captureRequest([]byte("GET /health HTTP/1.1\r\nHost: api.example.com\r\n\r\n"))

	entry := stream.requestLog()
	if entry.Method != "GET" {
		t.Fatalf("method = %q, want GET", entry.Method)
	}
	if entry.Address != "api.example.com/health" {
		t.Fatalf("address = %q, want api.example.com/health", entry.Address)
	}
	if entry.Target != "127.0.0.1:8080" {
		t.Fatalf("target = %q, want 127.0.0.1:8080", entry.Target)
	}
	if entry.Duration <= 0 {
		t.Fatalf("duration = %s, want positive", entry.Duration)
	}
}
