package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ConnectionType 定义连接支持的协议类型。
const (
	ConnectionTypeHTTP = "http"
	ConnectionTypeTCP  = "tcp"
	ConnectionTypeUDP  = "udp"
)

// ServerConfig 是服务端的完整配置，对应 server.yaml。
type ServerConfig struct {
	Server      ServerSection `yaml:"server"`
	TLS         TLSSection    `yaml:"tls"`
	Connections []Connection  `yaml:"connections"`
}

type ServerSection struct {
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	TunnelListen  string `yaml:"tunnel_listen"`
	HTTPListen    string `yaml:"http_listen"`
	HTTPSListen   string `yaml:"https_listen"`
	ConfigVersion int64  `yaml:"config_version"`
}

type TLSSection struct {
	Enabled         bool   `yaml:"enabled"`
	CertificateFile string `yaml:"certificate_file"`
	PrivateKeyFile  string `yaml:"private_key_file"`
}

// Connection 是统一的隧道连接定义，合并了客户端和转发规则。
type Connection struct {
	ID         string `yaml:"id"`
	Name       string `yaml:"name"`
	Type       string `yaml:"type"`           // "http"、"tcp" 或 "udp"，默认 "tcp"
	ListenPort int    `yaml:"listen_port"`    // TCP: 服务端监听端口; HTTP: 可为 0
	Host       string `yaml:"host,omitempty"` // HTTP: 虚拟主机名，可为空(匹配所有)
	// ForwardHost 仅对 HTTP 生效。true 时将客户端请求的域名 Host 原样转发到本地目标；
	// false 时由 HTTP 转发器使用本地目标地址作为 Host，保持默认行为。
	ForwardHost bool   `yaml:"forward_host,omitempty"`
	Target      string `yaml:"target"`     // 客户端转发目标地址 (如 localhost:8080)
	Token       string `yaml:"token"`      // 明文 Token，供 TUI 重复查看
	TokenHash   string `yaml:"token_hash"` // Argon2id 哈希
	Enabled     bool   `yaml:"enabled"`
	CreatedAt   string `yaml:"created_at,omitempty"`
}

// ClientConfig 是客户端本地配置。
type ClientConfig struct {
	// Server 和 Auth 保留以兼容旧版单隧道配置；新配置统一使用 Tunnels。
	Server  ServerRef      `yaml:"server,omitempty"`
	Auth    AuthRef        `yaml:"auth,omitempty"`
	TLS     ClientTLSRef   `yaml:"tls,omitempty"`
	Tunnels []ClientTunnel `yaml:"tunnels,omitempty"`
}

// ClientTunnel 是客户端持久化的一条服务端连接。
type ClientTunnel struct {
	ID        string `yaml:"id"`
	Name      string `yaml:"name,omitempty"`
	Server    string `yaml:"server"`
	Token     string `yaml:"token"`
	Enabled   bool   `yaml:"enabled"`
	CreatedAt string `yaml:"created_at,omitempty"`
}

type ClientTLSRef struct {
	Enabled    bool   `yaml:"enabled"`
	SkipVerify bool   `yaml:"skip_verify"`
	CAFile     string `yaml:"ca_file,omitempty"`
}

type ServerRef struct {
	Address string `yaml:"address"`
}

type AuthRef struct {
	Token string `yaml:"token"`
}

// LoadServerConfig 从文件加载服务端配置。
func LoadServerConfig(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg ServerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadClientConfig 从文件加载客户端配置。
func LoadClientConfig(path string) (*ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg ClientConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveServerConfig 将服务端配置以 YAML 写入文件。
func SaveServerConfig(path string, cfg *ServerConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// SaveClientConfig 将客户端配置以 YAML 写入文件。
func SaveClientConfig(path string, cfg *ClientConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if directory != "." {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0600)
}
