package configstore

import (
	"fmt"
	"strconv"
	"strings"

	"gopit/internal/config"
)

// Validate 校验服务端配置的完整性和一致性。
func Validate(cfg *config.ServerConfig) error {
	if cfg.Server.TunnelListen == "" {
		return fmt.Errorf("server.tunnel_listen is required")
	}

	// 连接 ID 唯一性 + TCP 监听端口唯一性
	connIDs := make(map[string]bool)
	tcpPorts := make(map[int]bool)
	udpPorts := make(map[int]bool)
	for i, c := range cfg.Connections {
		if c.ID == "" {
			return fmt.Errorf("connections[%d].id is required", i)
		}
		if connIDs[c.ID] {
			return fmt.Errorf("duplicate connection id: %s", c.ID)
		}
		connIDs[c.ID] = true

		if c.Name == "" {
			return fmt.Errorf("connection %s: name is required", c.ID)
		}

		// 默认类型为 tcp。
		connType := c.Type
		if connType == "" {
			connType = config.ConnectionTypeTCP
		}
		if connType != config.ConnectionTypeTCP && connType != config.ConnectionTypeHTTP && connType != config.ConnectionTypeUDP {
			return fmt.Errorf("connection %s: invalid type %q (must be tcp, http or udp)", c.ID, connType)
		}

		if connType == config.ConnectionTypeTCP || connType == config.ConnectionTypeUDP {
			if c.ListenPort < 1 || c.ListenPort > 65535 {
				return fmt.Errorf("connection %s: listen_port out of range (1-65535)", c.ID)
			}
			if connType == config.ConnectionTypeTCP && tcpPorts[c.ListenPort] {
				return fmt.Errorf("duplicate tcp listen_port: %d", c.ListenPort)
			}
			if connType == config.ConnectionTypeUDP && udpPorts[c.ListenPort] {
				return fmt.Errorf("duplicate %s listen_port: %d", connType, c.ListenPort)
			}
			if connType == config.ConnectionTypeTCP {
				tcpPorts[c.ListenPort] = true
			} else {
				udpPorts[c.ListenPort] = true
			}
		}
		if connType == config.ConnectionTypeHTTP && c.ListenPort > 0 {
			if c.ListenPort > 65535 {
				return fmt.Errorf("connection %s: listen_port out of range (1-65535)", c.ID)
			}
			if tcpPorts[c.ListenPort] {
				return fmt.Errorf("duplicate tcp/http listen_port: %d", c.ListenPort)
			}
			tcpPorts[c.ListenPort] = true
		}
		// HTTP 类型: listen_port 为 0 时使用全局 Web 端口，Host 可为空。

		if c.Target == "" {
			return fmt.Errorf("connection %s: target is required", c.ID)
		}
		if c.TokenHash == "" {
			return fmt.Errorf("connection %s: token_hash is required", c.ID)
		}
	}

	return nil
}

// ValidatePort 校验端口字符串。
func ValidatePort(portStr string) (int, error) {
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("invalid port: %s", portStr)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("port out of range (1-65535): %d", port)
	}
	return port, nil
}

// ValidateHost 校验域名格式（简单校验）。
func ValidateHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("host is empty")
	}
	if len(host) > 253 {
		return fmt.Errorf("host too long")
	}
	return nil
}
