package app

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopit/internal/config"
	"gopit/internal/protocol"
	"gopit/internal/server/auth"
	"gopit/internal/server/configstore"
	"gopit/internal/tunnel"
)

// ClientSession 跟踪一个已认证客户端的在线状态和隧道连接。
type ClientSession struct {
	ConnectionID string
	Name         string
	Tunnels      []*tunnel.TunnelConn
	RemoteAddr   string
	ConnectedAt  time.Time
	LastSeen     time.Time
	mu           sync.Mutex
}

func (s *ClientSession) addTunnel(tc *tunnel.TunnelConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Tunnels = append(s.Tunnels, tc)
	s.LastSeen = time.Now()
}

func (s *ClientSession) removeTunnel(tc *tunnel.TunnelConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.Tunnels {
		if t == tc {
			s.Tunnels = append(s.Tunnels[:i], s.Tunnels[i+1:]...)
			break
		}
	}
}

func (s *ClientSession) tunnelCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Tunnels)
}

// portListener 管理单个端口的 TCP 监听器。
type portListener struct {
	connID     string
	listenPort int
	listener   net.Listener
	streams    sync.Map // streamID -> net.Conn
	nextStream uint32
	mu         sync.Mutex
}

const configReloadInterval = 100 * time.Millisecond

// App 是服务端应用编排器。
type App struct {
	store     *configstore.Store
	logger    *slog.Logger
	tunnelCfg tunnel.Config

	mu       sync.RWMutex
	sessions map[string]*ClientSession // connectionID -> session

	// 端口监听器: connectionID -> *portListener
	portListeners     map[string]*portListener
	tunnelListener    net.Listener
	tunnelListen      string
	httpServer        *http.Server
	httpListen        string
	httpPortListeners map[string]*httpPortListener
	httpStreams       sync.Map // streamID -> *httpStream
	httpNextStream    atomic.Uint32
	stopCh            chan struct{}
	wg                sync.WaitGroup
}

// httpStream 保存 HTTP 请求对应的本地管道，响应数据通过隧道写入 serverConn。
type httpStream struct {
	serverConn net.Conn
	tunnel     *tunnel.TunnelConn
}

// httpPortListener 保存 HTTP 规则专属的直连监听器。
type httpPortListener struct {
	connID     string
	listenPort int
	listener   net.Listener
	server     *http.Server
}

// New 创建服务端 App。
func New(store *configstore.Store, logger *slog.Logger, tunnelCfg tunnel.Config) *App {
	return &App{
		store:             store,
		logger:            logger,
		tunnelCfg:         tunnelCfg,
		sessions:          make(map[string]*ClientSession),
		portListeners:     make(map[string]*portListener),
		httpPortListeners: make(map[string]*httpPortListener),
		stopCh:            make(chan struct{}),
	}
}

// Start 启动隧道监听并开始接受连接。
func (a *App) Start() error {
	cfg := a.store.Get()
	listener, err := a.createTunnelListener(cfg)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.tunnelListener = listener
	a.tunnelListen = cfg.Server.TunnelListen
	a.mu.Unlock()

	// 设置配置热加载回调
	a.store.OnConfigReloaded = a.onConfigReloaded
	a.store.StartWatcher(configReloadInterval)

	// 启动已配置连接的端口监听
	a.startPortListeners(cfg)
	a.startHTTPPortListeners(cfg)
	if err := a.startHTTPServer(cfg); err != nil {
		a.Stop()
		return err
	}

	a.wg.Add(1)
	go a.acceptLoop(listener)

	return nil
}

// startHTTPServer 启动真实 HTTP 访问入口；规则为空时仍返回 404，便于端口健康检查。
func (a *App) startHTTPServer(cfg *config.ServerConfig) error {
	if cfg.Server.HTTPListen == "" {
		return nil
	}
	listener, err := net.Listen("tcp", cfg.Server.HTTPListen)
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(a.handleHTTPRequest)}
	a.mu.Lock()
	a.httpServer = server
	a.httpListen = cfg.Server.HTTPListen
	a.mu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.logger.Info("http listener started", "addr", cfg.Server.HTTPListen)
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			a.logger.Error("http listener stopped unexpectedly", "err", serveErr)
		}
	}()
	return nil
}

// startHTTPPortListeners 为配置了独立端口的 HTTP 连接启动专属入口。
func (a *App) startHTTPPortListeners(cfg *config.ServerConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, conn := range cfg.Connections {
		if !conn.Enabled || conn.Type != config.ConnectionTypeHTTP || conn.ListenPort <= 0 {
			continue
		}
		if _, exists := a.httpPortListeners[conn.ID]; exists {
			continue
		}
		if err := a.startHTTPPortListenerLocked(conn, nil); err != nil {
			a.logger.Error("failed to start direct http listener", "conn_id", conn.ID, "port", conn.ListenPort, "err", err)
		}
	}
}

// startHTTPPortListenerLocked 注册指定 HTTP 连接的独立监听器；调用方必须持有 App 锁。
func (a *App) startHTTPPortListenerLocked(conn config.Connection, listener net.Listener) error {
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", fmt.Sprintf(":%d", conn.ListenPort))
		if err != nil {
			return err
		}
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		a.handleDirectHTTPRequest(writer, request, conn.ID)
	})}
	pl := &httpPortListener{connID: conn.ID, listenPort: conn.ListenPort, listener: listener, server: server}
	a.httpPortListeners[conn.ID] = pl
	a.logger.Info("direct http listener started", "conn_id", conn.ID, "name", conn.Name, "port", conn.ListenPort)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			a.logger.Error("direct http listener stopped unexpectedly", "conn_id", conn.ID, "port", conn.ListenPort, "err", err)
		}
	}()
	return nil
}

// stopHTTPPortListener 停止指定 HTTP 连接的独立监听器。
func (a *App) stopHTTPPortListener(connID string) {
	a.mu.Lock()
	pl, ok := a.httpPortListeners[connID]
	if ok {
		delete(a.httpPortListeners, connID)
	}
	a.mu.Unlock()
	if pl != nil {
		_ = pl.server.Close()
		a.logger.Info("direct http listener stopped", "conn_id", connID, "port", pl.listenPort)
	}
}

// createTunnelListener 根据当前 TLS 配置创建隧道监听器。
func (a *App) createTunnelListener(cfg *config.ServerConfig) (net.Listener, error) {
	if cfg.TLS.Enabled {
		tlsCfg, err := tunnel.NewServerTLSConfig(cfg.TLS.CertificateFile, cfg.TLS.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("server tls config: %w", err)
		}
		listener, err := tls.Listen("tcp", cfg.Server.TunnelListen, tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("tls listen: %w", err)
		}
		a.logger.Info("tunnel listener started (TLS)", "addr", cfg.Server.TunnelListen)
		return listener, nil
	}
	listener, err := net.Listen("tcp", cfg.Server.TunnelListen)
	if err != nil {
		return nil, fmt.Errorf("tcp listen: %w", err)
	}
	a.logger.Info("tunnel listener started (plain TCP)", "addr", cfg.Server.TunnelListen)
	return listener, nil
}

// startPortListeners 为所有启用的 TCP 连接启动端口监听。
func (a *App) startPortListeners(cfg *config.ServerConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, conn := range cfg.Connections {
		if !conn.Enabled {
			continue
		}
		// 仅 TCP 类型需要端口监听
		connType := conn.Type
		if connType == "" {
			connType = config.ConnectionTypeTCP
		}
		if connType != config.ConnectionTypeTCP {
			continue
		}
		if _, exists := a.portListeners[conn.ID]; exists {
			continue
		}
		if err := a.startPortListenerLocked(conn); err != nil {
			a.logger.Error("failed to start port listener", "conn_id", conn.ID, "port", conn.ListenPort, "err", err)
		}
	}
}

func (a *App) startPortListenerLocked(conn config.Connection) error {
	addr := fmt.Sprintf(":%d", conn.ListenPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	pl := &portListener{
		connID:     conn.ID,
		listenPort: conn.ListenPort,
		listener:   ln,
	}

	a.portListeners[conn.ID] = pl
	a.logger.Info("port listener started", "conn_id", conn.ID, "name", conn.Name, "port", conn.ListenPort)

	a.wg.Add(1)
	go a.portAcceptLoop(pl)

	return nil
}

// stopPortListener 停止指定连接的端口监听。
func (a *App) stopPortListener(connID string) {
	a.mu.Lock()
	pl, ok := a.portListeners[connID]
	if ok {
		delete(a.portListeners, connID)
	}
	a.mu.Unlock()

	if pl != nil {
		pl.listener.Close()
		a.logger.Info("port listener stopped", "conn_id", connID)
	}
}

// portAcceptLoop 接受外部连接并转发到客户端。
func (a *App) portAcceptLoop(pl *portListener) {
	defer a.wg.Done()
	for {
		conn, err := pl.listener.Accept()
		if err != nil {
			select {
			case <-a.stopCh:
				return
			default:
				a.logger.Error("port accept failed", "conn_id", pl.connID, "err", err)
				return
			}
		}
		a.wg.Add(1)
		go a.handlePortConn(pl, conn)
	}
}

// handlePortConn 处理外部连接，通过隧道转发到客户端。
func (a *App) handlePortConn(pl *portListener, conn net.Conn) {
	defer a.wg.Done()

	// 查找该连接对应的在线隧道
	a.mu.RLock()
	session, ok := a.sessions[pl.connID]
	a.mu.RUnlock()

	if !ok || session.tunnelCount() == 0 {
		a.logger.Warn("no online tunnel for port connection", "conn_id", pl.connID)
		conn.Close()
		return
	}

	// 获取第一条可用隧道
	session.mu.Lock()
	if len(session.Tunnels) == 0 {
		session.mu.Unlock()
		conn.Close()
		return
	}
	tc := session.Tunnels[0]
	session.mu.Unlock()

	// 分配 stream ID
	pl.mu.Lock()
	streamID := pl.nextStream
	pl.nextStream++
	pl.mu.Unlock()

	pl.streams.Store(streamID, conn)
	tc.IncActiveStream()

	// 发送 open_stream
	openData := protocol.EncodeOpenStream(pl.connID)
	tc.WriteData(&protocol.DataFrame{
		MsgType:  protocol.DataOpenStream,
		StreamID: streamID,
		Data:     openData,
	})

	a.logger.Debug("port connection opened", "conn_id", pl.connID, "stream_id", streamID, "remote", conn.RemoteAddr())

	// 读取外部连接数据并转发到隧道
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			tc.WriteData(&protocol.DataFrame{
				MsgType:  protocol.DataStreamData,
				StreamID: streamID,
				Data:     buf[:n],
			})
		}
		if err != nil {
			// 通知客户端关闭流
			tc.WriteData(&protocol.DataFrame{
				MsgType:  protocol.DataStreamClose,
				StreamID: streamID,
			})
			pl.streams.Delete(streamID)
			conn.Close()
			tc.DecActiveStream()
			return
		}
	}
}

// handleHTTPRequest 处理默认 Web 端口请求，仅根据 Host 匹配 HTTP 规则。
func (a *App) handleHTTPRequest(writer http.ResponseWriter, request *http.Request) {
	rule, found := a.findHTTPConnection(request.Host)
	if !found {
		http.Error(writer, "tunnel not found", http.StatusNotFound)
		return
	}
	a.forwardHTTPRequest(writer, request, rule)
}

// handleDirectHTTPRequest 处理 HTTP 规则专属端口请求，不依赖请求 Host。
func (a *App) handleDirectHTTPRequest(writer http.ResponseWriter, request *http.Request, connID string) {
	rule := a.findConnectionByID(connID)
	if rule.ID == "" || !rule.Enabled || rule.Type != config.ConnectionTypeHTTP || rule.ListenPort <= 0 {
		http.Error(writer, "tunnel not found", http.StatusNotFound)
		return
	}
	a.forwardHTTPRequest(writer, request, rule)
}

// forwardHTTPRequest 通过给定 HTTP 规则将完整请求和响应传输到客户端。
func (a *App) forwardHTTPRequest(writer http.ResponseWriter, request *http.Request, rule config.Connection) {
	serverConn, proxyConn := net.Pipe()
	defer proxyConn.Close()
	streamID, tc, err := a.openHTTPStream(rule.ID, serverConn)
	if err != nil {
		serverConn.Close()
		http.Error(writer, "tunnel unavailable", http.StatusBadGateway)
		return
	}
	go a.forwardHTTPStream(streamID, tc, serverConn)
	defer func() {
		_ = tc.WriteData(&protocol.DataFrame{MsgType: protocol.DataStreamClose, StreamID: streamID})
		a.closeHTTPStream(streamID)
	}()

	forwarded := request.Clone(request.Context())
	if !rule.ForwardHost {
		forwarded.Host = rule.Target
	}
	if err := forwarded.Write(proxyConn); err != nil {
		a.logger.Warn("write http request to tunnel failed", "conn_id", rule.ID, "err", err)
		http.Error(writer, "tunnel request failed", http.StatusBadGateway)
		return
	}
	response, err := http.ReadResponse(bufio.NewReader(proxyConn), forwarded)
	if err != nil {
		a.logger.Warn("read http response from tunnel failed", "conn_id", rule.ID, "err", err)
		http.Error(writer, "tunnel response failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	copyHTTPHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	if _, err := io.Copy(writer, response.Body); err != nil {
		a.logger.Debug("write http response failed", "conn_id", rule.ID, "err", err)
	}
}

// forwardHTTPStream 将 HTTP 服务端入口写入管道的请求字节发送给客户端隧道。
func (a *App) forwardHTTPStream(streamID uint32, tc *tunnel.TunnelConn, serverConn net.Conn) {
	buffer := make([]byte, 32*1024)
	for {
		count, readErr := serverConn.Read(buffer)
		if count > 0 {
			if writeErr := tc.WriteData(&protocol.DataFrame{
				MsgType:  protocol.DataStreamData,
				StreamID: streamID,
				Data:     buffer[:count],
			}); writeErr != nil {
				a.logger.Debug("write http stream data failed", "stream_id", streamID, "err", writeErr)
				return
			}
		}
		if readErr != nil {
			_ = tc.WriteData(&protocol.DataFrame{MsgType: protocol.DataStreamClose, StreamID: streamID})
			return
		}
	}
}

// findHTTPConnection 使用请求域名匹配启用的 HTTP 规则。
func (a *App) findHTTPConnection(host string) (config.Connection, bool) {
	requestHost := normalizeHTTPHost(host)
	for _, connection := range a.store.Get().Connections {
		if !connection.Enabled || connection.Type != config.ConnectionTypeHTTP {
			continue
		}
		if connection.Host == "" {
			continue
		}
		if normalizeHTTPHost(connection.Host) == requestHost {
			return connection, true
		}
	}
	return config.Connection{}, false
}

func normalizeHTTPHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		return parsedHost
	}
	return host
}

func copyHTTPHeaders(destination, source http.Header) {
	for name, values := range source {
		if strings.EqualFold(name, "Transfer-Encoding") || strings.EqualFold(name, "Connection") {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

// openHTTPStream 选择在线客户端隧道并注册 HTTP 请求的响应回写管道。
func (a *App) openHTTPStream(connID string, serverConn net.Conn) (uint32, *tunnel.TunnelConn, error) {
	a.mu.RLock()
	session := a.sessions[connID]
	a.mu.RUnlock()
	if session == nil || session.tunnelCount() == 0 {
		return 0, nil, fmt.Errorf("no online tunnel")
	}
	session.mu.Lock()
	if len(session.Tunnels) == 0 {
		session.mu.Unlock()
		return 0, nil, fmt.Errorf("no active tunnel")
	}
	tc := session.Tunnels[0]
	session.mu.Unlock()
	streamID := a.httpNextStream.Add(1)
	a.httpStreams.Store(streamID, &httpStream{serverConn: serverConn, tunnel: tc})
	tc.IncActiveStream()
	if err := tc.WriteData(&protocol.DataFrame{MsgType: protocol.DataOpenStream, StreamID: streamID, Data: protocol.EncodeOpenStream(connID)}); err != nil {
		a.httpStreams.Delete(streamID)
		tc.DecActiveStream()
		return 0, nil, err
	}
	return streamID, tc, nil
}

// closeHTTPStream 仅回收一次 HTTP 流及其活动流计数。
func (a *App) closeHTTPStream(streamID uint32) {
	if value, loaded := a.httpStreams.LoadAndDelete(streamID); loaded {
		entry := value.(*httpStream)
		_ = entry.serverConn.Close()
		entry.tunnel.DecActiveStream()
	}
}

// Stop 停止服务端应用。
func (a *App) Stop() {
	close(a.stopCh)
	a.mu.RLock()
	tunnelListener := a.tunnelListener
	a.mu.RUnlock()
	if tunnelListener != nil {
		tunnelListener.Close()
	}
	a.mu.RLock()
	httpServer := a.httpServer
	a.mu.RUnlock()
	if httpServer != nil {
		httpServer.Close()
	}

	// 停止所有端口监听
	a.mu.Lock()
	for _, pl := range a.httpPortListeners {
		_ = pl.server.Close()
	}
	a.httpPortListeners = make(map[string]*httpPortListener)
	for _, pl := range a.portListeners {
		pl.listener.Close()
	}
	a.portListeners = make(map[string]*portListener)

	// 关闭隧道会触发 OnClose 回调，该回调会申请 App 锁，因此先收集再释放锁。
	tunnels := make([]*tunnel.TunnelConn, 0)
	for _, s := range a.sessions {
		s.mu.Lock()
		tunnels = append(tunnels, s.Tunnels...)
		s.Tunnels = nil
		s.mu.Unlock()
	}
	a.mu.Unlock()
	for _, tc := range tunnels {
		tc.Close()
	}

	a.store.Close()
	a.wg.Wait()
}

func (a *App) acceptLoop(listener net.Listener) {
	defer a.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-a.stopCh:
				return
			default:
				if !a.isCurrentTunnelListener(listener) {
					return
				}
				a.logger.Error("accept failed", "err", err)
				continue
			}
		}
		a.wg.Add(1)
		go a.handleConn(conn)
	}
}

// isCurrentTunnelListener 判断监听器是否仍为当前活动实例。
func (a *App) isCurrentTunnelListener(listener net.Listener) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.tunnelListener == listener
}

func (a *App) handleConn(conn net.Conn) {
	defer a.wg.Done()

	// 1. 读取 auth 帧
	authMsg, err := tunnel.ServerReadAuth(conn, a.tunnelCfg)
	if err != nil {
		a.logger.Info("auth read failed", "remote", conn.RemoteAddr(), "err", err)
		conn.Close()
		return
	}

	// 2. 验证 Token，匹配到 Connection
	connID, ok := a.verifyToken(authMsg.Token)
	if !ok {
		a.logger.Info("auth failed", "remote", conn.RemoteAddr())
		tunnel.ServerSendAuthResult(conn, false, "", "invalid token")
		conn.Close()
		return
	}

	// 3. 发送 auth_result
	if err := tunnel.ServerSendAuthResult(conn, true, connID, ""); err != nil {
		a.logger.Error("send auth_result failed", "err", err)
		conn.Close()
		return
	}

	a.logger.Info("client authenticated", "conn_id", connID, "remote", conn.RemoteAddr())

	// 4. 创建 TunnelConn
	tc := tunnel.NewTunnelConn(conn, a.tunnelCfg, a.logger)
	tc.SetClientID(connID)

	tc.OnControl = func(msg *protocol.ControlMessage, t *tunnel.TunnelConn) {
		a.handleControl(msg, t)
	}
	tc.OnData = func(df *protocol.DataFrame, t *tunnel.TunnelConn) {
		a.handleData(df, t)
	}
	tc.OnClose = func(t *tunnel.TunnelConn) {
		a.removeTunnelFromSession(connID, t)
	}

	// 5. 注册会话
	session := a.getOrCreateSession(connID, conn.RemoteAddr().String())
	session.addTunnel(tc)

	// 6. 启动隧道
	tc.Start()

	// 7. 发送规则快照（告诉客户端转发目标）
	a.sendRulesSnapshot(tc, connID)
}

// verifyToken 验证 Token 并返回匹配的 Connection ID。
func (a *App) verifyToken(token string) (string, bool) {
	cfg := a.store.Get()
	for _, c := range cfg.Connections {
		if !c.Enabled {
			continue
		}
		if auth.VerifyToken(token, c.TokenHash) {
			return c.ID, true
		}
	}
	return "", false
}

func (a *App) handleControl(msg *protocol.ControlMessage, tc *tunnel.TunnelConn) {
	switch msg.Type {
	case protocol.MsgTypeRulesSyncReq:
		a.logger.Debug("client requested rules sync", "conn_id", tc.ClientID())
		a.sendRulesSnapshot(tc, tc.ClientID())
	case protocol.MsgTypeError:
		a.logger.Warn("client reported error", "conn_id", tc.ClientID(), "code", msg.Code, "message", msg.Message)
	}
}

// handleData 处理从客户端返回的数据帧。
func (a *App) handleData(df *protocol.DataFrame, tc *tunnel.TunnelConn) {
	connID := tc.ClientID()

	a.mu.RLock()
	pl, ok := a.portListeners[connID]
	a.mu.RUnlock()

	if !ok {
		a.handleHTTPStreamData(df)
		return
	}

	switch df.MsgType {
	case protocol.DataStreamData:
		// 查找对应的本地连接并写入数据
		v, ok := pl.streams.Load(df.StreamID)
		if !ok {
			a.logger.Warn("data for unknown stream", "stream_id", df.StreamID)
			return
		}
		conn := v.(net.Conn)
		if len(df.Data) > 0 {
			conn.Write(df.Data)
		}

	case protocol.DataStreamClose:
		v, ok := pl.streams.Load(df.StreamID)
		if ok {
			conn := v.(net.Conn)
			conn.Close()
			pl.streams.Delete(df.StreamID)
		}

	case protocol.DataHalfClose:
		v, ok := pl.streams.Load(df.StreamID)
		if ok {
			conn := v.(net.Conn)
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				tcpConn.CloseRead()
			}
		}
	}
}

// handleHTTPStreamData 将客户端响应数据写入对应 HTTP 请求的管道。
func (a *App) handleHTTPStreamData(df *protocol.DataFrame) {
	value, ok := a.httpStreams.Load(df.StreamID)
	if !ok {
		a.logger.Warn("data frame for unknown stream", "stream_id", df.StreamID)
		return
	}
	stream := value.(*httpStream)
	switch df.MsgType {
	case protocol.DataStreamData:
		if len(df.Data) > 0 {
			_, _ = stream.serverConn.Write(df.Data)
		}
	case protocol.DataStreamClose:
		a.closeHTTPStream(df.StreamID)
	case protocol.DataHalfClose:
		if tcpConn, isTCP := stream.serverConn.(*net.TCPConn); isTCP {
			_ = tcpConn.CloseRead()
		}
	}
}

// sendRulesSnapshot 向客户端发送转发目标信息。
func (a *App) sendRulesSnapshot(tc *tunnel.TunnelConn, connID string) {
	cfg := a.store.Get()

	var routes []protocol.RouteEntry
	for _, c := range cfg.Connections {
		if c.ID == connID && c.Enabled {
			routes = append(routes, protocol.RouteEntry{
				ID:          c.ID,
				Name:        c.Name,
				Protocol:    c.Type,
				Host:        c.Host,
				ForwardHost: c.ForwardHost,
				Target:      c.Target,
			})
		}
	}

	version := cfg.Server.ConfigVersion
	msg := protocol.NewRulesSnapshot(version, routes)
	if err := tc.WriteControl(msg); err != nil {
		a.logger.Error("send rules snapshot failed", "conn_id", connID, "err", err)
	} else {
		a.logger.Info("sent rules snapshot", "conn_id", connID, "routes", len(routes), "version", version)
	}
}

// notifyClient 发送 rules_changed 通知。
func (a *App) notifyClient(connID string) {
	a.mu.RLock()
	session, ok := a.sessions[connID]
	a.mu.RUnlock()
	if !ok {
		return
	}

	version := a.store.GetConfigVersion()
	msg := protocol.NewRulesChanged(version)

	session.mu.Lock()
	tunnels := make([]*tunnel.TunnelConn, len(session.Tunnels))
	copy(tunnels, session.Tunnels)
	session.mu.Unlock()

	for _, tc := range tunnels {
		if !tc.IsClosed() {
			if err := tc.WriteControl(msg); err != nil {
				a.logger.Error("notify client failed", "conn_id", connID, "err", err)
			}
		}
	}
}

func (a *App) onConfigReloaded(newCfg *config.ServerConfig) {
	a.logger.Info("config reloaded, applying listeners and rules")
	if err := a.restartTunnelListener(newCfg); err != nil {
		a.logger.Error("restart tunnel listener failed, keeping previous listener", "addr", newCfg.Server.TunnelListen, "err", err)
	}
	if err := a.restartHTTPServer(newCfg); err != nil {
		a.logger.Error("restart http listener failed, keeping previous listener", "addr", newCfg.Server.HTTPListen, "err", err)
	}

	// 停止已删除或禁用的端口监听
	a.mu.RLock()
	existingIDs := make(map[string]bool)
	for _, c := range newCfg.Connections {
		existingIDs[c.ID] = true
	}
	a.mu.RUnlock()

	// 停止不再存在的连接
	a.mu.RLock()
	currentIDs := make([]string, 0, len(a.portListeners))
	for id := range a.portListeners {
		currentIDs = append(currentIDs, id)
	}
	a.mu.RUnlock()

	for _, id := range currentIDs {
		conn, found := a.findConnection(newCfg, id)
		a.mu.RLock()
		pl := a.portListeners[id]
		a.mu.RUnlock()
		connType := conn.Type
		if connType == "" {
			connType = config.ConnectionTypeTCP
		}
		if !found || pl == nil || !conn.Enabled || connType != config.ConnectionTypeTCP || pl.listenPort != conn.ListenPort {
			a.stopPortListener(id)
		}
	}

	// 启动新的端口监听
	a.startPortListeners(newCfg)
	a.syncHTTPPortListeners(newCfg)

	// 通知所有在线客户端
	a.mu.RLock()
	connIDs := make([]string, 0, len(a.sessions))
	for id := range a.sessions {
		connIDs = append(connIDs, id)
	}
	a.mu.RUnlock()

	for _, id := range connIDs {
		a.notifyClient(id)
	}
}

// syncHTTPPortListeners 根据最新配置增量更新 HTTP 规则的独立端口监听。
func (a *App) syncHTTPPortListeners(cfg *config.ServerConfig) {
	a.mu.RLock()
	currentIDs := make([]string, 0, len(a.httpPortListeners))
	for id := range a.httpPortListeners {
		currentIDs = append(currentIDs, id)
	}
	a.mu.RUnlock()

	for _, id := range currentIDs {
		conn, found := a.findConnection(cfg, id)
		a.mu.RLock()
		pl := a.httpPortListeners[id]
		a.mu.RUnlock()
		if !found || pl == nil || !conn.Enabled || conn.Type != config.ConnectionTypeHTTP || conn.ListenPort <= 0 || pl.listenPort != conn.ListenPort {
			a.stopHTTPPortListener(id)
		}
	}
	a.startHTTPPortListeners(cfg)
}

// restartHTTPServer 在 HTTP 监听地址变更后先绑定新端口，再关闭旧入口。
func (a *App) restartHTTPServer(cfg *config.ServerConfig) error {
	a.mu.RLock()
	currentAddress := a.httpListen
	currentServer := a.httpServer
	a.mu.RUnlock()
	if currentAddress == cfg.Server.HTTPListen {
		return nil
	}
	if cfg.Server.HTTPListen == "" {
		if currentServer != nil {
			_ = currentServer.Close()
		}
		a.mu.Lock()
		a.httpServer = nil
		a.httpListen = ""
		a.mu.Unlock()
		return nil
	}
	listener, err := net.Listen("tcp", cfg.Server.HTTPListen)
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(a.handleHTTPRequest)}
	a.mu.Lock()
	a.httpServer = server
	a.httpListen = cfg.Server.HTTPListen
	a.mu.Unlock()
	if currentServer != nil {
		_ = currentServer.Close()
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.logger.Info("http listener reloaded", "addr", cfg.Server.HTTPListen)
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			a.logger.Error("http listener stopped unexpectedly", "err", serveErr)
		}
	}()
	return nil
}

// restartTunnelListener 在隧道监听地址变更后无中断切换新监听器。
func (a *App) restartTunnelListener(cfg *config.ServerConfig) error {
	a.mu.RLock()
	currentAddress := a.tunnelListen
	a.mu.RUnlock()
	if currentAddress == cfg.Server.TunnelListen {
		return nil
	}

	newListener, err := a.createTunnelListener(cfg)
	if err != nil {
		return err
	}

	a.mu.Lock()
	oldListener := a.tunnelListener
	a.tunnelListener = newListener
	a.tunnelListen = cfg.Server.TunnelListen
	a.mu.Unlock()
	if oldListener != nil {
		oldListener.Close()
	}
	a.wg.Add(1)
	go a.acceptLoop(newListener)
	a.logger.Info("tunnel listener reloaded", "addr", cfg.Server.TunnelListen)
	return nil
}

func (a *App) findConnection(cfg *config.ServerConfig, id string) (config.Connection, bool) {
	for _, c := range cfg.Connections {
		if c.ID == id {
			return c, true
		}
	}
	return config.Connection{}, false
}

func (a *App) getOrCreateSession(connID, remoteAddr string) *ClientSession {
	a.mu.Lock()
	defer a.mu.Unlock()

	session, ok := a.sessions[connID]
	if !ok {
		cfg := a.store.Get()
		name := connID
		for _, c := range cfg.Connections {
			if c.ID == connID {
				name = c.Name
				break
			}
		}
		session = &ClientSession{
			ConnectionID: connID,
			Name:         name,
			ConnectedAt:  time.Now(),
			LastSeen:     time.Now(),
		}
		a.sessions[connID] = session
	}
	session.RemoteAddr = remoteAddr
	return session
}

func (a *App) removeTunnelFromSession(connID string, tc *tunnel.TunnelConn) {
	a.mu.Lock()
	session, ok := a.sessions[connID]
	a.mu.Unlock()
	if !ok {
		return
	}

	session.removeTunnel(tc)

	if session.tunnelCount() == 0 {
		a.mu.Lock()
		delete(a.sessions, connID)
		a.mu.Unlock()
		a.logger.Info("client offline", "conn_id", connID)
	}
}

// --- TUI 调用的管理方法 ---

// CreateConnection 创建新连接并返回明文 Token。
// connType 为 HTTP、TCP 或 UDP。TCP/UDP 类型需要 listenPort > 0。
func (a *App) CreateConnection(name, connType, host string, listenPort int, target string, forwardHost bool, customToken string) (connID, token string, err error) {
	if connType == "" {
		connType = config.ConnectionTypeTCP
	}

	connID, err = auth.GenerateClientID()
	if err != nil {
		return "", "", err
	}

	if customToken != "" {
		token = customToken
	} else {
		token, err = auth.GenerateToken()
		if err != nil {
			return "", "", err
		}
	}

	tokenHash, err := auth.HashToken(token)
	if err != nil {
		return "", "", err
	}

	// TCP 类型先尝试绑定端口
	var ln net.Listener
	if connType == config.ConnectionTypeTCP && listenPort > 0 {
		ln, err = net.Listen("tcp", fmt.Sprintf(":%d", listenPort))
		if err != nil {
			return "", "", fmt.Errorf("监听端口 %d 失败: %w", listenPort, err)
		}
	}

	err = a.store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
		cfg.Connections = append(cfg.Connections, config.Connection{
			ID:          connID,
			Name:        name,
			Type:        connType,
			Host:        host,
			ForwardHost: forwardHost,
			ListenPort:  listenPort,
			Target:      target,
			Token:       token,
			TokenHash:   tokenHash,
			Enabled:     true,
			CreatedAt:   time.Now().Format(time.RFC3339),
		})
		return cfg, nil
	})
	if err != nil {
		if ln != nil {
			ln.Close()
		}
		return "", "", err
	}

	// TCP 类型: 配置写入成功，注册监听器
	if ln != nil {
		a.mu.Lock()
		pl := &portListener{
			connID:     connID,
			listenPort: listenPort,
			listener:   ln,
		}
		a.portListeners[connID] = pl
		a.mu.Unlock()

		a.logger.Info("port listener started", "conn_id", connID, "name", name, "type", connType, "port", listenPort)
		a.wg.Add(1)
		go a.portAcceptLoop(pl)
	} else {
		a.logger.Info("connection created", "conn_id", connID, "name", name, "type", connType, "host", host)
	}
	a.onConfigReloaded(a.store.Get())

	return connID, token, nil
}

// UpdateConnection 修改连接配置。
func (a *App) UpdateConnection(connID, name, connType, host string, listenPort int, target string, forwardHost bool) error {
	if connType == "" {
		connType = config.ConnectionTypeTCP
	}

	oldConn := a.findConnectionByID(connID)
	if oldConn.ID == "" {
		return fmt.Errorf("connection not found: %s", connID)
	}

	// TCP 类型: 如果端口变更，先尝试绑定新端口
	var newLn net.Listener
	needRestart := false
	if connType == config.ConnectionTypeTCP && oldConn.Enabled {
		if oldConn.Type != config.ConnectionTypeTCP || oldConn.ListenPort != listenPort {
			needRestart = true
			var err error
			newLn, err = net.Listen("tcp", fmt.Sprintf(":%d", listenPort))
			if err != nil {
				return fmt.Errorf("监听端口 %d 失败: %w", listenPort, err)
			}
		}
	}

	err := a.store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
		for i := range cfg.Connections {
			if cfg.Connections[i].ID == connID {
				cfg.Connections[i].Name = name
				cfg.Connections[i].Type = connType
				cfg.Connections[i].Host = host
				cfg.Connections[i].ForwardHost = forwardHost
				cfg.Connections[i].ListenPort = listenPort
				cfg.Connections[i].Target = target
				return cfg, nil
			}
		}
		return nil, fmt.Errorf("connection not found: %s", connID)
	})
	if err != nil {
		if newLn != nil {
			newLn.Close()
		}
		return err
	}

	if needRestart {
		a.stopPortListener(connID)
		a.mu.Lock()
		pl := &portListener{
			connID:     connID,
			listenPort: listenPort,
			listener:   newLn,
		}
		a.portListeners[connID] = pl
		a.mu.Unlock()
		a.logger.Info("port listener restarted", "conn_id", connID, "name", name, "port", listenPort)
		a.wg.Add(1)
		go a.portAcceptLoop(pl)
	}

	// 类型从 TCP 变为非 TCP 时，停止旧的端口监听。
	if oldConn.Type == config.ConnectionTypeTCP && connType != config.ConnectionTypeTCP {
		a.stopPortListener(connID)
	}
	a.onConfigReloaded(a.store.Get())

	return nil
}

// findConnectionByID 按 ID 查找连接配置。
func (a *App) findConnectionByID(connID string) config.Connection {
	cfg := a.store.Get()
	for _, c := range cfg.Connections {
		if c.ID == connID {
			return c
		}
	}
	return config.Connection{}
}

// UpdateConnectionToken 设置连接的 Token（手动或自动生成）。
func (a *App) UpdateConnectionToken(connID string, customToken string) (string, error) {
	var token string
	var err error

	if customToken != "" {
		token = customToken
	} else {
		token, err = auth.GenerateToken()
		if err != nil {
			return "", err
		}
	}

	tokenHash, err := auth.HashToken(token)
	if err != nil {
		return "", err
	}

	err = a.store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
		for i := range cfg.Connections {
			if cfg.Connections[i].ID == connID {
				cfg.Connections[i].Token = token
				cfg.Connections[i].TokenHash = tokenHash
				return cfg, nil
			}
		}
		return nil, fmt.Errorf("connection not found: %s", connID)
	})

	if err == nil {
		a.disconnectConnection(connID)
	}

	return token, err
}

// SetConnectionEnabled 启用或禁用连接。
func (a *App) SetConnectionEnabled(connID string, enabled bool) error {
	// 启用时: TCP 类型先尝试绑定端口
	var newLn net.Listener
	isTCP := false
	if enabled {
		conn := a.findConnectionByID(connID)
		if conn.ID == "" {
			return fmt.Errorf("connection not found: %s", connID)
		}
		connType := conn.Type
		if connType == "" {
			connType = config.ConnectionTypeTCP
		}
		if connType == config.ConnectionTypeTCP {
			isTCP = true
			ln, err := net.Listen("tcp", fmt.Sprintf(":%d", conn.ListenPort))
			if err != nil {
				return fmt.Errorf("监听端口 %d 失败: %w", conn.ListenPort, err)
			}
			newLn = ln
		}
	}

	err := a.store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
		for i := range cfg.Connections {
			if cfg.Connections[i].ID == connID {
				cfg.Connections[i].Enabled = enabled
				return cfg, nil
			}
		}
		return nil, fmt.Errorf("connection not found: %s", connID)
	})
	if err != nil {
		if newLn != nil {
			newLn.Close()
		}
		return err
	}

	if enabled {
		if isTCP && newLn != nil {
			a.mu.Lock()
			pl := &portListener{
				connID:     connID,
				listenPort: a.findConnectionByID(connID).ListenPort,
				listener:   newLn,
			}
			a.portListeners[connID] = pl
			a.mu.Unlock()
			a.logger.Info("port listener started", "conn_id", connID, "port", a.findConnectionByID(connID).ListenPort)
			a.wg.Add(1)
			go a.portAcceptLoop(pl)
		}
	} else {
		a.stopPortListener(connID)
		a.disconnectConnection(connID)
	}
	a.onConfigReloaded(a.store.Get())
	return nil
}

// DeleteConnection 删除连接。
func (a *App) DeleteConnection(connID string) error {
	err := a.store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
		result := cfg.Connections[:0]
		deleted := false
		for _, c := range cfg.Connections {
			if c.ID == connID {
				deleted = true
				continue
			}
			result = append(result, c)
		}
		if !deleted {
			return nil, fmt.Errorf("connection not found: %s", connID)
		}
		cfg.Connections = result
		return cfg, nil
	})

	if err == nil {
		a.stopPortListener(connID)
		a.stopHTTPPortListener(connID)
		a.disconnectConnection(connID)
		a.onConfigReloaded(a.store.Get())
	}
	return err
}

// UpdateServerConfig 修改服务端实际监听地址。
func (a *App) UpdateServerConfig(tunnelListen, httpListen, httpsListen string) error {
	err := a.store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
		cfg.Server.TunnelListen = tunnelListen
		cfg.Server.HTTPListen = httpListen
		cfg.Server.HTTPSListen = httpsListen
		return cfg, nil
	})
	if err == nil {
		a.onConfigReloaded(a.store.Get())
	}
	return err
}

func (a *App) disconnectConnection(connID string) {
	a.mu.Lock()
	session, ok := a.sessions[connID]
	a.mu.Unlock()
	if !ok {
		return
	}

	session.mu.Lock()
	tunnels := make([]*tunnel.TunnelConn, len(session.Tunnels))
	copy(tunnels, session.Tunnels)
	session.Tunnels = nil
	session.mu.Unlock()

	for _, tc := range tunnels {
		tc.Close()
	}

	a.mu.Lock()
	delete(a.sessions, connID)
	a.mu.Unlock()
}

// --- 查询方法（供 TUI 使用）---

// TunnelDetail 是单条隧道的详细信息。
type TunnelDetail struct {
	RemoteAddr    string
	ConnectedAt   time.Time
	BytesSent     int64
	BytesReceived int64
	ActiveStreams int32
	TotalStreams  int32
	LastPongAt    time.Time
}

// OnlineClientInfo 是在线连接的概要信息。
type OnlineClientInfo struct {
	ConnectionID  string
	Name          string
	RemoteAddr    string
	TunnelCount   int
	ConnectedAt   time.Time
	LastSeen      time.Time
	BytesSent     int64
	BytesReceived int64
	ActiveStreams int32
	TotalStreams  int32
	Tunnels       []TunnelDetail
}

// GetOnlineClients 返回当前在线连接列表。
func (a *App) GetOnlineClients() []OnlineClientInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := make([]OnlineClientInfo, 0, len(a.sessions))
	for _, s := range a.sessions {
		s.mu.Lock()
		info := OnlineClientInfo{
			ConnectionID: s.ConnectionID,
			Name:         s.Name,
			RemoteAddr:   s.RemoteAddr,
			TunnelCount:  len(s.Tunnels),
			ConnectedAt:  s.ConnectedAt,
			LastSeen:     s.LastSeen,
		}
		for _, tc := range s.Tunnels {
			ts := tc.GetStats()
			info.BytesSent += ts.BytesSent
			info.BytesReceived += ts.BytesReceived
			info.ActiveStreams += ts.ActiveStreams
			info.TotalStreams += ts.TotalStreams
			info.Tunnels = append(info.Tunnels, TunnelDetail{
				RemoteAddr:    tc.RemoteAddr(),
				ConnectedAt:   ts.ConnectedAt,
				BytesSent:     ts.BytesSent,
				BytesReceived: ts.BytesReceived,
				ActiveStreams: ts.ActiveStreams,
				TotalStreams:  ts.TotalStreams,
				LastPongAt:    time.Unix(0, tc.LastPongTime()),
			})
		}
		s.mu.Unlock()
		result = append(result, info)
	}
	return result
}

// GetStats 返回概览统计信息。
type Stats struct {
	OnlineConnections int
	TotalConnections  int
	ActiveTunnels     int
	ActiveStreams     int32
	BytesSent         int64
	BytesReceived     int64
}

func (a *App) GetStats() Stats {
	cfg := a.store.Get()
	a.mu.RLock()
	defer a.mu.RUnlock()

	activeTunnels := 0
	var totalBytesSent, totalBytesReceived int64
	var totalActiveStreams int32
	for _, s := range a.sessions {
		s.mu.Lock()
		activeTunnels += len(s.Tunnels)
		for _, tc := range s.Tunnels {
			ts := tc.GetStats()
			totalBytesSent += ts.BytesSent
			totalBytesReceived += ts.BytesReceived
			totalActiveStreams += ts.ActiveStreams
		}
		s.mu.Unlock()
	}

	return Stats{
		OnlineConnections: len(a.sessions),
		TotalConnections:  len(cfg.Connections),
		ActiveTunnels:     activeTunnels,
		ActiveStreams:     totalActiveStreams,
		BytesSent:         totalBytesSent,
		BytesReceived:     totalBytesReceived,
	}
}

// EnsureDataDir 确保数据目录存在。
func EnsureDataDir(path string) error {
	return os.MkdirAll(path, 0700)
}
