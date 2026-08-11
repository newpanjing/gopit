package agent

import (
	"io"
	"log/slog"
	"testing"

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
