package tunnel

import (
	"fmt"
	"net"
	"time"

	"gopit/internal/protocol"
)

// ClientHandshake 在客户端侧完成鉴权握手。
// 发送 auth 帧，等待 auth_result。
// 成功时返回 auth_result 消息；失败时返回错误。
func ClientHandshake(conn net.Conn, clientID, token string, info *protocol.ClientInfo, cfg Config) (*protocol.ControlMessage, error) {
	// 发送 auth
	authMsg := protocol.NewAuth(int(protocol.ProtocolVersion), clientID, token, info)
	if err := protocol.WriteControlFrame(conn, authMsg); err != nil {
		return nil, fmt.Errorf("send auth: %w", err)
	}

	// 读取 auth_result，设置超时
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	frame, err := protocol.ReadFrame(conn, cfg.MaxControlFrameSize, cfg.MaxDataPayloadSize)
	if err != nil {
		return nil, fmt.Errorf("read auth_result: %w", err)
	}
	if frame.Kind != protocol.KindControl {
		return nil, fmt.Errorf("expected control frame, got kind=%d", frame.Kind)
	}

	msg, err := protocol.DecodeControlPayload(frame.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode auth_result: %w", err)
	}
	if msg.Type != protocol.MsgTypeAuthResult {
		return nil, fmt.Errorf("expected auth_result, got %s", msg.Type)
	}
	if !msg.Success {
		return msg, fmt.Errorf("%w: %s", ErrAuthFailed, msg.Error)
	}
	return msg, nil
}

// ServerReadAuth 在服务端侧读取客户端的 auth 帧。
func ServerReadAuth(conn net.Conn, cfg Config) (*protocol.ControlMessage, error) {
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	frame, err := protocol.ReadFrame(conn, cfg.MaxControlFrameSize, cfg.MaxDataPayloadSize)
	if err != nil {
		return nil, fmt.Errorf("read auth: %w", err)
	}
	if frame.Kind != protocol.KindControl {
		return nil, fmt.Errorf("expected control frame, got kind=%d", frame.Kind)
	}

	msg, err := protocol.DecodeControlPayload(frame.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode auth: %w", err)
	}
	if msg.Type != protocol.MsgTypeAuth {
		return nil, fmt.Errorf("expected auth, got %s", msg.Type)
	}

	// 校验协议版本
	if msg.Version != int(protocol.ProtocolVersion) {
		return nil, fmt.Errorf("%w: client=%d server=%d", ErrProtocolVersion, msg.Version, protocol.ProtocolVersion)
	}

	return msg, nil
}

// ServerSendAuthResult 在服务端侧发送 auth_result 帧。
func ServerSendAuthResult(conn net.Conn, success bool, clientID, errMsg string) error {
	resultMsg := protocol.NewAuthResult(success, clientID, errMsg)
	return protocol.WriteControlFrame(conn, resultMsg)
}
