package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestRenderTunnelRowAlignment 验证表头和数据行的转发、服务器列使用同一固定起始位置。
func TestRenderTunnelRowAlignment(t *testing.T) {
	header := renderTunnelRow("", "Name", "Forward", "Server", "Enabled", "Status", "Streams", "Rate")
	row := renderTunnelRow("▸", "production", "192.168.1.200:80", "192.168.1.188:7001", "enabled", "connected", "0", "↑0B/s ↓0B/s")

	for _, value := range []string{"Forward", "Server"} {
		if strings.Index(header, value) < 0 {
			t.Fatalf("header missing %q: %q", value, header)
		}
	}
	if got, want := displayOffset(row, "192.168.1.200:80"), displayOffset(header, "Forward"); got != want {
		t.Fatalf("forward column = %d, want %d", got, want)
	}
	if got, want := displayOffset(row, "192.168.1.188:7001"), displayOffset(header, "Server"); got != want {
		t.Fatalf("server column = %d, want %d", got, want)
	}
}

// displayOffset 返回文本在终端中的显示起始列，处理选择符号的多字节编码。
func displayOffset(line, value string) int {
	return lipgloss.Width(line[:strings.Index(line, value)])
}
