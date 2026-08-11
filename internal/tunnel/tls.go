package tunnel

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// NewServerTLSConfig 为服务端创建 TLS 配置。
func NewServerTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert/key: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// NewClientTLSConfig 为客户端创建 TLS 配置。
// skipVerify 为 true 时跳过证书验证（仅用于开发环境）。
func NewClientTLSConfig(skipVerify bool, caFile string) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if skipVerify {
		cfg.InsecureSkipVerify = true
		return cfg, nil
	}
	if caFile != "" {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read ca file: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		cfg.RootCAs = caPool
	}
	return cfg, nil
}
