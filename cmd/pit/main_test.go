package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopit/internal/config"
	"gopit/internal/runtime"
)

// TestCompareVersions 验证升级版本仅在远端版本更高时才判定可升级。
func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name       string
		latest     string
		current    string
		comparison int
		valid      bool
	}{
		{name: "patch upgrade", latest: "v1.2.11", current: "v1.2.10", comparison: 1, valid: true},
		{name: "already latest", latest: "v1.2.10", current: "v1.2.10", comparison: 0, valid: true},
		{name: "remote older", latest: "v1.2.9", current: "v1.2.10", comparison: -1, valid: true},
		{name: "development build", latest: "v1.2.10", current: "dev", comparison: 0, valid: false},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			comparison, valid := compareVersions(item.latest, item.current)
			if comparison != item.comparison || valid != item.valid {
				t.Fatalf("compareVersions(%q, %q) = (%d, %t), want (%d, %t)", item.latest, item.current, comparison, valid, item.comparison, item.valid)
			}
		})
	}
}

// TestFormatDownloadBytes 验证升级进度使用正确的二进制单位。
func TestFormatDownloadBytes(t *testing.T) {
	if value := formatDownloadBytes(1024); value != "1.0KB" {
		t.Fatalf("formatDownloadBytes(1024) = %q, want 1.0KB", value)
	}
}

// TestDefaultClientConfigPath 验证新客户端默认使用用户目录中的专属配置文件。
func TestDefaultClientConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, userDataDirectoryName, clientConfigFileName)
	if got := defaultClientConfigPath(); got != want {
		t.Fatalf("defaultClientConfigPath() = %q, want %q", got, want)
	}
}

// TestDefaultServerConfigPath 验证新服务端默认使用用户目录中的专属配置文件。
func TestDefaultServerConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, userDataDirectoryName, serverConfigFileName)
	if got := defaultServerConfigPath(); got != want {
		t.Fatalf("defaultServerConfigPath() = %q, want %q", got, want)
	}
}

// TestReleaseTagFromPath 验证 API 限流时可从 GitHub 页面重定向路径识别版本。
func TestReleaseTagFromPath(t *testing.T) {
	tag, ok := releaseTagFromPath("/newpanjing/gopit/releases/tag/v1.2.12")
	if !ok || tag != "v1.2.12" {
		t.Fatalf("releaseTagFromPath() = (%q, %t), want (v1.2.12, true)", tag, ok)
	}
	if _, ok := releaseTagFromPath("/newpanjing/gopit/releases/latest"); ok {
		t.Fatal("latest redirect path unexpectedly parsed as a release tag")
	}
}

// TestSelectTUIMode 验证首次进入 TUI 时可纠正无效输入并选择客户端模式。
func TestSelectTUIMode(t *testing.T) {
	var output bytes.Buffer
	mode, err := selectTUIMode(strings.NewReader("x\n2\n"), &output)
	if err != nil {
		t.Fatalf("selectTUIMode() error = %v", err)
	}
	if mode != runtime.ModeClient {
		t.Fatalf("selectTUIMode() = %q, want %q", mode, runtime.ModeClient)
	}
}

// TestEnsureClientConfig 验证首次选择客户端模式时会创建用户级配置目录和文件。
func TestEnsureClientConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), userDataDirectoryName, clientConfigFileName)
	if err := ensureClientConfig(path); err != nil {
		t.Fatalf("ensureClientConfig() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("client config was not created: %v", err)
	}
}

// TestAddClientTunnel 验证命令行添加的隧道会持久化，并拒绝重复的服务端与 Token 组合。
func TestAddClientTunnel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	server := "43.128.61.60:7001"
	token := "example-token"

	if err := addClientTunnel(path, server, token); err != nil {
		t.Fatalf("addClientTunnel() error = %v", err)
	}
	cfg, err := config.LoadClientConfig(path)
	if err != nil {
		t.Fatalf("load client config: %v", err)
	}
	if len(cfg.Tunnels) != 1 {
		t.Fatalf("tunnel count = %d, want 1", len(cfg.Tunnels))
	}
	if got := cfg.Tunnels[0]; got.Server != server || got.Token != token || !got.Enabled || got.ID == "" {
		t.Fatalf("stored tunnel = %#v, want enabled tunnel for %s", got, server)
	}
	if err := addClientTunnel(path, server, token); err == nil {
		t.Fatal("duplicate tunnel was accepted")
	}
}

// TestDownloadReleaseAssetRetries 验证升级下载遇到截断响应时会重新请求完整文件。
func TestDownloadReleaseAssetRetries(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			writer.Header().Set("Content-Length", "6")
			_, _ = writer.Write([]byte("bad"))
			return
		}
		_, _ = writer.Write([]byte("complete"))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "pit")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatalf("create temporary asset file: %v", err)
	}
	if err := downloadReleaseAsset(server.URL, path); err != nil {
		t.Fatalf("downloadReleaseAsset() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read downloaded asset: %v", err)
	}
	if string(data) != "complete" || attempts != 2 {
		t.Fatalf("downloaded data = %q, attempts = %d; want complete after 2 attempts", data, attempts)
	}
}
