package auth

import (
	"encoding/base64"
	"testing"
)

// TestGenerateToken 验证生成的短 Token 保持 URL 安全格式和哈希校验能力。
func TestGenerateToken(t *testing.T) {
	token, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken() error = %v", err)
	}
	if len(token) != 22 {
		t.Fatalf("token length = %d, want 22", len(token))
	}
	if _, err := base64.RawURLEncoding.DecodeString(token); err != nil {
		t.Fatalf("token is not URL-safe Base64: %v", err)
	}
	hash, err := HashToken(token)
	if err != nil {
		t.Fatalf("HashToken() error = %v", err)
	}
	if !VerifyToken(token, hash) {
		t.Fatal("VerifyToken() = false, want true")
	}
}
