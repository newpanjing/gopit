package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	// Argon2id 参数
	argonMemory      = 64 * 1024 // 64MB
	argonIterations  = 3
	argonParallelism = 2
	argonSaltLength  = 16
	argonKeyLength   = 32

	// Token 长度（字节数，hex 编码后 64 字符）
	tokenBytes = 32
)

// GenerateToken 生成高强度的随机 Token 明文。
func GenerateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// GenerateClientID 生成客户端 ID。
func GenerateClientID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate client id: %w", err)
	}
	return "client-" + hex.EncodeToString(b), nil
}

// HashToken 使用 Argon2id 对 Token 进行哈希，返回 PHC 格式字符串。
// 格式: $argon2id$v=19$m=65536,t=3,p=2$<base64-salt>$<base64-hash>
func HashToken(token string) (string, error) {
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	hash := argon2.IDKey([]byte(token), salt, argonIterations, argonMemory, argonParallelism, argonKeyLength)

	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)

	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonIterations, argonParallelism, b64Salt, b64Hash), nil
}

// VerifyToken 在恒定时间内验证 Token 是否匹配哈希。
func VerifyToken(token, encodedHash string) bool {
	// 解析 PHC 格式
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 {
		return false
	}
	if parts[1] != "argon2id" {
		return false
	}

	var memory uint32
	var iterations uint32
	var parallelism uint8
	_, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism)
	if err != nil {
		return false
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}

	expectedHash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}

	// 使用相同参数重新计算哈希
	actualHash := argon2.IDKey([]byte(token), salt, iterations, memory, parallelism, uint32(len(expectedHash)))

	// 恒定时间比较
	return subtle.ConstantTimeCompare(actualHash, expectedHash) == 1
}
