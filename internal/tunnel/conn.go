package tunnel

import (
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"gopit/internal/protocol"
)

// Config 是隧道连接的运行参数。
type Config struct {
	HeartbeatInterval   time.Duration
	HeartbeatTimeout    time.Duration
	MaxControlFrameSize int
	MaxDataPayloadSize  int
	WriteQueueSize      int
}

// DefaultConfig 返回默认隧道配置。
func DefaultConfig() Config {
	return Config{
		HeartbeatInterval:   30 * time.Second,
		HeartbeatTimeout:    90 * time.Second,
		MaxControlFrameSize: protocol.MaxControlFrameSize,
		MaxDataPayloadSize:  protocol.MaxDataPayloadSize,
		WriteQueueSize:      256,
	}
}

// TunnelStats 是隧道连接的实时统计信息。
type TunnelStats struct {
	BytesSent     int64
	BytesReceived int64
	ActiveStreams int32
	TotalStreams  int32
	ConnectedAt   time.Time
}

// TunnelConn 是基于 TLS/TCP 的帧级隧道连接。
// 单一写协程保证帧完整性，读协程分发控制帧和数据帧。
type TunnelConn struct {
	conn   net.Conn
	cfg    Config
	logger *slog.Logger

	writeCh   chan *protocol.Frame
	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool

	// 回调
	OnControl func(msg *protocol.ControlMessage, tc *TunnelConn)
	OnData    func(df *protocol.DataFrame, tc *TunnelConn)
	OnError   func(err error, tc *TunnelConn)
	OnClose   func(tc *TunnelConn)

	// 状态
	remoteAddr string
	clientID   string
	lastPong   atomic.Int64 // unix nano

	// 统计
	bytesSent     atomic.Int64
	bytesReceived atomic.Int64
	activeStreams atomic.Int32
	totalStreams  atomic.Int32
	connectedAt   time.Time
}

// NewTunnelConn 创建隧道连接包装器。
func NewTunnelConn(conn net.Conn, cfg Config, logger *slog.Logger) *TunnelConn {
	tc := &TunnelConn{
		conn:        conn,
		cfg:         cfg,
		logger:      logger,
		writeCh:     make(chan *protocol.Frame, cfg.WriteQueueSize),
		done:        make(chan struct{}),
		remoteAddr:  conn.RemoteAddr().String(),
		connectedAt: time.Now(),
	}
	tc.lastPong.Store(time.Now().UnixNano())
	return tc
}

// Start 启动读、写和心跳协程。
func (tc *TunnelConn) Start() {
	go tc.readLoop()
	go tc.writeLoop()
	go tc.heartbeatLoop()
}

// RemoteAddr 返回远端地址。
func (tc *TunnelConn) RemoteAddr() string {
	return tc.remoteAddr
}

// ClientID 返回已认证的客户端 ID。
func (tc *TunnelConn) ClientID() string {
	return tc.clientID
}

// SetClientID 设置已认证的客户端 ID。
func (tc *TunnelConn) SetClientID(id string) {
	tc.clientID = id
}

// LastPongTime 返回最后一次收到 pong 的时间（unix nano）。
func (tc *TunnelConn) LastPongTime() int64 {
	return tc.lastPong.Load()
}

// IsClosed 返回连接是否已关闭。
func (tc *TunnelConn) IsClosed() bool {
	return tc.closed.Load()
}

// Done 返回隧道关闭时触发的只读通道，供调用方等待连接生命周期结束。
func (tc *TunnelConn) Done() <-chan struct{} {
	return tc.done
}

// GetStats 返回当前隧道的统计信息快照。
func (tc *TunnelConn) GetStats() TunnelStats {
	return TunnelStats{
		BytesSent:     tc.bytesSent.Load(),
		BytesReceived: tc.bytesReceived.Load(),
		ActiveStreams: tc.activeStreams.Load(),
		TotalStreams:  tc.totalStreams.Load(),
		ConnectedAt:   tc.connectedAt,
	}
}

// ConnectedAt 返回连接建立时间。
func (tc *TunnelConn) ConnectedAt() time.Time {
	return tc.connectedAt
}

// IncActiveStream 增加活动流计数。
func (tc *TunnelConn) IncActiveStream() {
	tc.activeStreams.Add(1)
	tc.totalStreams.Add(1)
}

// DecActiveStream 减少活动流计数。
func (tc *TunnelConn) DecActiveStream() {
	tc.activeStreams.Add(-1)
}

// WriteControl 异步发送控制帧。
func (tc *TunnelConn) WriteControl(msg protocol.ControlMessage) error {
	data, err := protocol.MarshalControl(msg)
	if err != nil {
		return err
	}
	frame := &protocol.Frame{
		Version: protocol.ProtocolVersion,
		Kind:    protocol.KindControl,
		Payload: data,
	}
	return tc.sendFrame(frame)
}

// WriteData 异步发送数据帧。
func (tc *TunnelConn) WriteData(df *protocol.DataFrame) error {
	payload := protocol.EncodeDataFrame(df)
	frame := &protocol.Frame{
		Version: protocol.ProtocolVersion,
		Kind:    protocol.KindData,
		Payload: payload,
	}
	return tc.sendFrame(frame)
}

func (tc *TunnelConn) sendFrame(frame *protocol.Frame) error {
	if tc.closed.Load() {
		return ErrTunnelClosed
	}
	select {
	case tc.writeCh <- frame:
		return nil
	case <-tc.done:
		return ErrTunnelClosed
	}
}

// Close 关闭隧道连接。
func (tc *TunnelConn) Close() error {
	tc.closeOnce.Do(func() {
		tc.closed.Store(true)
		close(tc.done)
		tc.conn.Close()
		if tc.OnClose != nil {
			tc.OnClose(tc)
		}
	})
	return nil
}

func (tc *TunnelConn) readLoop() {
	for {
		frame, err := protocol.ReadFrame(tc.conn, tc.cfg.MaxControlFrameSize, tc.cfg.MaxDataPayloadSize)
		if err != nil {
			if !tc.closed.Load() {
				tc.logger.Debug("tunnel read error", "addr", tc.remoteAddr, "err", err)
				if tc.OnError != nil {
					tc.OnError(err, tc)
				}
			}
			tc.Close()
			return
		}

		// 统计接收字节数
		tc.bytesReceived.Add(int64(len(frame.Payload) + protocol.HeaderSize))

		switch frame.Kind {
		case protocol.KindControl:
			msg, err := protocol.DecodeControlPayload(frame.Payload)
			if err != nil {
				tc.logger.Warn("decode control frame failed", "err", err)
				continue
			}
			// 处理内置的 ping/pong
			if msg.Type == protocol.MsgTypePong {
				tc.lastPong.Store(time.Now().UnixNano())
				continue
			}
			if msg.Type == protocol.MsgTypePing {
				// 自动回复 pong
				tc.WriteControl(protocol.NewPong(msg.Timestamp))
				continue
			}
			if tc.OnControl != nil {
				tc.OnControl(msg, tc)
			}
		case protocol.KindData:
			df, err := protocol.DecodeDataFrame(frame.Payload)
			if err != nil {
				tc.logger.Warn("decode data frame failed", "err", err)
				continue
			}
			if tc.OnData != nil {
				tc.OnData(df, tc)
			}
		}
	}
}

func (tc *TunnelConn) writeLoop() {
	for {
		select {
		case <-tc.done:
			return
		case frame := <-tc.writeCh:
			// 统计发送字节数
			tc.bytesSent.Add(int64(len(frame.Payload) + protocol.HeaderSize))
			if err := protocol.WriteFrame(tc.conn, frame); err != nil {
				if !tc.closed.Load() {
					tc.logger.Debug("tunnel write error", "addr", tc.remoteAddr, "err", err)
					if tc.OnError != nil {
						tc.OnError(err, tc)
					}
				}
				tc.Close()
				return
			}
		}
	}
}

func (tc *TunnelConn) heartbeatLoop() {
	ticker := time.NewTicker(tc.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-tc.done:
			return
		case <-ticker.C:
			// 发送 ping
			tc.WriteControl(protocol.NewPing(time.Now().UnixNano()))

			// 检查 pong 超时
			lastPong := time.Unix(0, tc.lastPong.Load())
			if time.Since(lastPong) > tc.cfg.HeartbeatTimeout {
				tc.logger.Info("heartbeat timeout, closing tunnel",
					"addr", tc.remoteAddr,
					"client_id", tc.clientID,
					"last_pong", lastPong.Format(time.RFC3339),
				)
				tc.Close()
				return
			}
		}
	}
}
