package agent

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gopit/internal/config"
	"gopit/internal/protocol"
	"gopit/internal/tunnel"
)

// StatusInfo 是客户端当前运行状态。
type StatusInfo struct {
	Status        string // "connecting", "connected", "auth_failed", "disconnected"
	ConnectionID  string
	RemoteAddr    string
	ConnectedAt   time.Time
	LastHeartbeat time.Time
	Error         string
	Name          string
	Target        string
	ConfigVersion int64
	BytesSent     int64
	BytesReceived int64
	ActiveStreams int32
	TotalStreams  int32
}

// EventInfo 是客户端隧道的完整运行事件，供后台管理器记录与展示。
type EventInfo struct {
	Time    time.Time
	Message string
}

// streamConn 管理单个数据流的双向转发。
type streamConn struct {
	localConn net.Conn
	tc        *tunnel.TunnelConn
	streamID  uint32
	done      chan struct{}
}

// Agent 管理客户端到服务端的隧道连接。
type Agent struct {
	cfg       config.ClientConfig
	cfgPath   string
	logger    *slog.Logger
	tunnelCfg tunnel.Config

	mu            sync.RWMutex
	tunnel        *tunnel.TunnelConn
	connected     atomic.Bool
	connectionID  string
	name          string
	target        string
	configVersion int64
	lastHeartbeat time.Time

	// 活动流
	streams sync.Map // streamID -> *streamConn

	// 回调
	OnStatusChange func(info StatusInfo)
	OnEvent        func(event EventInfo)

	stopCh   chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// New 创建客户端 Agent。
func New(cfg config.ClientConfig, cfgPath string, logger *slog.Logger, tunnelCfg tunnel.Config) *Agent {
	return &Agent{
		cfg:       cfg,
		cfgPath:   cfgPath,
		logger:    logger,
		tunnelCfg: tunnelCfg,
		stopCh:    make(chan struct{}),
	}
}

// Start 启动连接循环。
func (a *Agent) Start() {
	a.wg.Add(1)
	go a.connectLoop()
}

// Stop 停止 Agent。
func (a *Agent) Stop() {
	a.stopOnce.Do(func() {
		close(a.stopCh)
		tc := a.currentTunnel()
		if tc != nil {
			tc.Close()
		}
		a.wg.Wait()
	})
}

// Reconnect 手动触发重连。
func (a *Agent) Reconnect() {
	tc := a.currentTunnel()
	if tc != nil {
		tc.Close()
	}
}

// currentTunnel 返回当前隧道快照，调用方必须在不持有 Agent 锁时关闭它。
func (a *Agent) currentTunnel() *tunnel.TunnelConn {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.tunnel
}

func (a *Agent) connectLoop() {
	defer a.wg.Done()

	backoff := 2 * time.Second
	maxBackoff := 60 * time.Second

	for {
		select {
		case <-a.stopCh:
			return
		default:
		}

		a.notifyStatus("connecting", "")

		err := a.connect()
		if err != nil {
			a.logger.Warn("connection failed", "err", err)
			if err == tunnel.ErrAuthFailed {
				a.notifyStatus("auth_failed", err.Error())
			} else {
				a.notifyStatus("disconnected", err.Error())
			}

			jitter := time.Duration(rand.Int63n(int64(backoff) / 2))
			wait := backoff + jitter

			select {
			case <-a.stopCh:
				return
			case <-time.After(wait):
			}

			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		backoff = 2 * time.Second

		a.mu.Lock()
		tc := a.tunnel
		a.mu.Unlock()

		if tc != nil {
			select {
			case <-a.stopCh:
				return
			case <-tc.Done():
			}
		}

		a.connected.Store(false)
		a.notifyStatus("disconnected", "")
	}
}

func (a *Agent) connect() error {
	address := a.cfg.Server.Address
	token := a.cfg.Auth.Token

	if token == "" {
		return fmt.Errorf("token not configured")
	}

	// 建立 TLS/TCP 连接
	var conn net.Conn
	var err error

	if a.cfg.TLS.Enabled {
		tlsCfg, err := tunnel.NewClientTLSConfig(a.cfg.TLS.SkipVerify, a.cfg.TLS.CAFile)
		if err != nil {
			return fmt.Errorf("tls config: %w", err)
		}
		conn, err = tls.Dial("tcp", address, tlsCfg)
	} else {
		conn, err = net.Dial("tcp", address)
	}
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	// 客户端元信息
	info := &protocol.ClientInfo{
		OS:       runtimeOS(),
		Arch:     runtimeArch(),
		Hostname: hostname(),
	}

	// 鉴权握手
	authResult, err := tunnel.ClientHandshake(conn, "", token, info, a.tunnelCfg)
	if err != nil {
		conn.Close()
		return err
	}

	connID := authResult.ClientID
	a.logger.Info("authenticated", "conn_id", connID, "server", address)
	a.notifyEvent("authenticated")

	// 创建隧道
	tc := tunnel.NewTunnelConn(conn, a.tunnelCfg, a.logger)
	tc.SetClientID(connID)

	tc.OnControl = func(msg *protocol.ControlMessage, t *tunnel.TunnelConn) {
		a.handleControl(msg, t)
	}
	tc.OnData = func(df *protocol.DataFrame, t *tunnel.TunnelConn) {
		a.handleData(df, t)
	}
	tc.OnError = func(tunnelErr error, t *tunnel.TunnelConn) {
		a.logger.Warn("tunnel transport error", "conn_id", t.ClientID(), "err", tunnelErr)
	}
	tc.OnClose = func(t *tunnel.TunnelConn) {
		a.mu.Lock()
		if a.tunnel == t {
			a.tunnel = nil
		}
		a.mu.Unlock()
		a.connected.Store(false)
		a.logger.Warn("tunnel disconnected", "conn_id", t.ClientID())
		// 关闭所有活动流
		a.streams.Range(func(key, value any) bool {
			if sc, ok := value.(*streamConn); ok {
				sc.localConn.Close()
			}
			a.streams.Delete(key)
			return true
		})
	}

	a.mu.Lock()
	a.tunnel = tc
	a.connectionID = connID
	a.lastHeartbeat = time.Now()
	a.mu.Unlock()

	a.connected.Store(true)
	a.notifyStatus("connected", "")
	a.logger.Info("tunnel connected", "conn_id", connID, "server", address)
	a.notifyEvent("tunnel connected")

	tc.Start()

	return nil
}

func (a *Agent) handleControl(msg *protocol.ControlMessage, tc *tunnel.TunnelConn) {
	switch msg.Type {
	case protocol.MsgTypeRulesSnapshot:
		a.applyRoutes(msg.Routes, msg.ConfigVersion)
		a.logger.Info("received configuration snapshot", "routes", len(msg.Routes), "version", msg.ConfigVersion)
		a.notifyEvent(fmt.Sprintf("received configuration snapshot, routes=%d version=%d", len(msg.Routes), msg.ConfigVersion))

	case protocol.MsgTypeRulesChanged:
		a.logger.Info("received configuration change", "version", msg.ConfigVersion)
		a.notifyEvent(fmt.Sprintf("received configuration change, version=%d", msg.ConfigVersion))
		tc.WriteControl(protocol.NewRulesSyncRequest())
	}
}

// handleData 处理从服务端收到的数据帧。
func (a *Agent) handleData(df *protocol.DataFrame, tc *tunnel.TunnelConn) {
	switch df.MsgType {
	case protocol.DataOpenStream:
		// 服务端发起新流，解析 target 并建立本地连接
		target, err := protocol.DecodeOpenStream(df.Data)
		if err != nil {
			a.logger.Error("decode open_stream failed", "err", err)
			return
		}

		// 使用配置的 target（从 rules snapshot 收到）
		a.mu.RLock()
		resolvedTarget := a.target
		a.mu.RUnlock()

		// 如果 target 为空，尝试使用规则中的 target
		if resolvedTarget == "" {
			resolvedTarget = target
		}
		a.logger.Info("received forwarding request", "stream_id", df.StreamID, "target", resolvedTarget)
		a.notifyEvent(fmt.Sprintf("received forwarding request, stream=%d target=%s", df.StreamID, resolvedTarget))

		if resolvedTarget == "" {
			a.logger.Error("no target configured for stream", "stream_id", df.StreamID)
			tc.WriteData(&protocol.DataFrame{
				MsgType:  protocol.DataStreamClose,
				StreamID: df.StreamID,
			})
			return
		}

		// 建立到目标的本地连接
		localConn, err := net.Dial("tcp", resolvedTarget)
		if err != nil {
			a.logger.Error("dial target failed", "target", resolvedTarget, "err", err)
			tc.WriteData(&protocol.DataFrame{
				MsgType:  protocol.DataStreamClose,
				StreamID: df.StreamID,
			})
			return
		}

		sc := &streamConn{
			localConn: localConn,
			tc:        tc,
			streamID:  df.StreamID,
			done:      make(chan struct{}),
		}

		a.streams.Store(df.StreamID, sc)
		tc.IncActiveStream()

		a.logger.Info("forwarding request connected", "stream_id", df.StreamID, "target", resolvedTarget)
		a.notifyEvent(fmt.Sprintf("forwarding request connected, stream=%d", df.StreamID))

		// 启动本地读取协程
		go a.streamLocalToTunnel(sc)

	case protocol.DataStreamData:
		v, ok := a.streams.Load(df.StreamID)
		if !ok {
			a.logger.Warn("data for unknown stream", "stream_id", df.StreamID)
			return
		}
		sc := v.(*streamConn)
		if len(df.Data) > 0 {
			sc.localConn.Write(df.Data)
		}

	case protocol.DataStreamClose:
		v, ok := a.streams.Load(df.StreamID)
		if ok {
			sc := v.(*streamConn)
			sc.localConn.Close()
			a.streams.Delete(df.StreamID)
			tc.DecActiveStream()
			a.logger.Info("forwarding request closed", "stream_id", df.StreamID)
			a.notifyEvent(fmt.Sprintf("forwarding request closed, stream=%d", df.StreamID))
		}

	case protocol.DataHalfClose:
		v, ok := a.streams.Load(df.StreamID)
		if ok {
			sc := v.(*streamConn)
			if tcpConn, ok := sc.localConn.(*net.TCPConn); ok {
				tcpConn.CloseRead()
			}
		}
	}
}

// streamLocalToTunnel 从本地连接读取数据并发送到隧道。
func (a *Agent) streamLocalToTunnel(sc *streamConn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := sc.localConn.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			sc.tc.WriteData(&protocol.DataFrame{
				MsgType:  protocol.DataStreamData,
				StreamID: sc.streamID,
				Data:     data,
			})
		}
		if err != nil {
			sc.tc.WriteData(&protocol.DataFrame{
				MsgType:  protocol.DataStreamClose,
				StreamID: sc.streamID,
			})
			sc.localConn.Close()
			a.streams.Delete(sc.streamID)
			sc.tc.DecActiveStream()
			return
		}
	}
}

func (a *Agent) applyRoutes(routes []protocol.RouteEntry, version int64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.configVersion = version
	for _, r := range routes {
		a.name = r.Name
		a.target = r.Target
		break // 只有一个 target
	}
}

// GetStatus 返回当前状态信息。
func (a *Agent) GetStatus() StatusInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	info := StatusInfo{
		Status:        a.getStatus(),
		ConnectionID:  a.connectionID,
		RemoteAddr:    a.cfg.Server.Address,
		LastHeartbeat: a.lastHeartbeat,
		Name:          a.name,
		Target:        a.target,
		ConfigVersion: a.configVersion,
	}
	if a.tunnel != nil {
		ts := a.tunnel.GetStats()
		info.ConnectedAt = ts.ConnectedAt
		info.BytesSent = ts.BytesSent
		info.BytesReceived = ts.BytesReceived
		info.ActiveStreams = ts.ActiveStreams
		info.TotalStreams = ts.TotalStreams
	}
	return info
}

func (a *Agent) getStatus() string {
	if a.connected.Load() {
		return "connected"
	}
	return "disconnected"
}

func (a *Agent) notifyStatus(status, errMsg string) {
	info := a.GetStatus()
	info.Status = status
	info.Error = errMsg
	if a.OnStatusChange != nil {
		a.OnStatusChange(info)
	}
}

// notifyEvent 将非状态类事件推送给管理器；日志仍由调用处保留。
func (a *Agent) notifyEvent(message string) {
	if a.OnEvent != nil {
		a.OnEvent(EventInfo{Time: time.Now(), Message: message})
	}
}

// SaveConfig 保存客户端配置到文件。
func SaveConfig(path string, address, token string) error {
	cfg := config.ClientConfig{Tunnels: []config.ClientTunnel{{
		ID: "default", Name: "default", Server: address, Token: token, Enabled: true,
	}}}
	return config.SaveClientConfig(path, &cfg)
}

// --- 辅助函数 ---

func runtimeOS() string {
	return "linux"
}

func runtimeArch() string {
	return "amd64"
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
