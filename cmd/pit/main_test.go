package main

import "testing"

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
