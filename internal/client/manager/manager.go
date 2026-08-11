// Package manager 管理同一客户端配置中的多个后台隧道连接。
package manager

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gopit/internal/client/agent"
	"gopit/internal/config"
	"gopit/internal/tunnel"
)

const configReloadInterval = 100 * time.Millisecond
const maxEvents = 100

// TunnelStatus 是提供给管理界面的单条隧道实时状态。
type TunnelStatus struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Server   string           `json:"server"`
	Enabled  bool             `json:"enabled"`
	Status   agent.StatusInfo `json:"status"`
	SendRate int64            `json:"send_rate"`
	RecvRate int64            `json:"recv_rate"`
}

// Event 是客户端运行过程中的可查看事件。
type Event struct {
	Time     time.Time     `json:"time"`
	TunnelID string        `json:"tunnel_id"`
	Message  string        `json:"message"`
	Method   string        `json:"method,omitempty"`
	Address  string        `json:"address,omitempty"`
	Target   string        `json:"target,omitempty"`
	Duration time.Duration `json:"duration,omitempty"`
}

// Snapshot 是客户端后台管理器的实时快照。
type Snapshot struct {
	Tunnels []TunnelStatus `json:"tunnels"`
	Events  []Event        `json:"events"`
}

type runningTunnel struct {
	definition config.ClientTunnel
	agent      *agent.Agent
}

type transferSample struct {
	time     time.Time
	sent     int64
	received int64
}

// Manager 持续加载客户端配置，并将差异实时应用到 Agent 集合。
type Manager struct {
	configPath string
	logger     *slog.Logger
	tunnelCfg  tunnel.Config
	mu         sync.RWMutex
	running    map[string]runningTunnel
	events     []Event
	samples    map[string]transferSample
	stopCh     chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
}

// New 根据配置路径创建客户端管理器。
func New(configPath string, logger *slog.Logger, tunnelCfg tunnel.Config) *Manager {
	return &Manager{configPath: configPath, logger: logger, tunnelCfg: tunnelCfg, running: make(map[string]runningTunnel), samples: make(map[string]transferSample), stopCh: make(chan struct{})}
}

// Start 启动配置同步与全部已启用隧道。
func (m *Manager) Start() error {
	if err := m.reconcile(); err != nil {
		return err
	}
	m.wg.Add(1)
	go m.watchLoop()
	return nil
}

// Stop 停止所有隧道和配置监控。
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
		m.wg.Wait()
		m.mu.Lock()
		running := m.running
		m.running = make(map[string]runningTunnel)
		m.mu.Unlock()
		for _, item := range running {
			item.agent.Stop()
		}
	})
}

func (m *Manager) watchLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(configReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			if err := m.reconcile(); err != nil {
				m.logger.Warn("reload client configuration failed", "err", err)
			}
		}
	}
}

func (m *Manager) reconcile() error {
	cfg, err := config.LoadClientConfig(m.configPath)
	if err != nil {
		return fmt.Errorf("load client config: %w", err)
	}
	desired := clientTunnels(cfg)
	wanted := make(map[string]config.ClientTunnel, len(desired))
	for _, item := range desired {
		if item.ID != "" {
			wanted[item.ID] = item
		}
	}

	m.mu.Lock()
	toStop := make([]runningTunnel, 0)
	for id, current := range m.running {
		wantedItem, exists := wanted[id]
		if !exists || !wantedItem.Enabled || !sameDefinition(current.definition, wantedItem) {
			toStop = append(toStop, current)
			delete(m.running, id)
		}
	}
	toStart := make([]config.ClientTunnel, 0)
	for id, item := range wanted {
		if item.Enabled {
			if _, exists := m.running[id]; !exists {
				toStart = append(toStart, item)
			}
		}
	}
	m.mu.Unlock()
	for _, current := range toStop {
		current.agent.Stop()
		m.addEvent(current.definition.ID, "tunnel stopped")
	}
	for _, item := range toStart {
		m.startTunnel(item)
	}
	return nil
}

func (m *Manager) startTunnel(item config.ClientTunnel) {
	cfg := config.ClientConfig{Server: config.ServerRef{Address: item.Server}, Auth: config.AuthRef{Token: item.Token}}
	a := agent.New(cfg, m.configPath, m.logger, m.tunnelCfg)
	a.OnStatusChange = func(info agent.StatusInfo) { m.addEvent(item.ID, statusMessage(info)) }
	a.OnEvent = func(event agent.EventInfo) { m.addAgentEvent(item.ID, event) }
	m.mu.Lock()
	m.running[item.ID] = runningTunnel{definition: item, agent: a}
	m.mu.Unlock()
	a.Start()
	m.addEvent(item.ID, "tunnel started")
}

// GetSnapshot 返回运行状态、流量统计和最近事件。
func (m *Manager) GetSnapshot() Snapshot {
	cfg, err := config.LoadClientConfig(m.configPath)
	if err != nil {
		return Snapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result := Snapshot{Tunnels: make([]TunnelStatus, 0, len(clientTunnels(cfg))), Events: append([]Event(nil), m.events...)}
	for _, item := range clientTunnels(cfg) {
		status := agent.StatusInfo{Status: "stopped", RemoteAddr: item.Server}
		if current, ok := m.running[item.ID]; ok {
			status = current.agent.GetStatus()
		}
		// 客户端名称由服务端规则快照决定，本地仅保留隧道 ID 供管理器匹配。
		displayName := status.Name
		if displayName == "" {
			displayName = "-"
		}
		sendRate, recvRate := m.rateFor(item.ID, status.BytesSent, status.BytesReceived)
		result.Tunnels = append(result.Tunnels, TunnelStatus{ID: item.ID, Name: displayName, Server: item.Server, Enabled: item.Enabled, Status: status, SendRate: sendRate, RecvRate: recvRate})
	}
	return result
}

func (m *Manager) addEvent(id, message string) {
	m.addEventAt(id, time.Now(), message)
}

func (m *Manager) addEventAt(id string, eventTime time.Time, message string) {
	m.addEventRecord(Event{Time: eventTime, TunnelID: id, Message: message})
}

// addAgentEvent 保留客户端 Agent 上报的请求字段，供日志页按列展示。
func (m *Manager) addAgentEvent(id string, event agent.EventInfo) {
	m.addEventRecord(Event{Time: event.Time, TunnelID: id, Message: event.Message, Method: event.Method, Address: event.Address, Target: event.Target, Duration: event.Duration})
}

func (m *Manager) addEventRecord(event Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
	if len(m.events) > maxEvents {
		m.events = append([]Event(nil), m.events[len(m.events)-maxEvents:]...)
	}
}

func (m *Manager) rateFor(id string, sent, received int64) (int64, int64) {
	now := time.Now()
	previous, exists := m.samples[id]
	m.samples[id] = transferSample{time: now, sent: sent, received: received}
	if !exists || now.Sub(previous.time) <= 0 {
		return 0, 0
	}
	seconds := now.Sub(previous.time).Seconds()
	return int64(float64(maxInt64(0, sent-previous.sent)) / seconds), int64(float64(maxInt64(0, received-previous.received)) / seconds)
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func clientTunnels(cfg *config.ClientConfig) []config.ClientTunnel {
	if len(cfg.Tunnels) > 0 {
		return cfg.Tunnels
	}
	if cfg.Server.Address == "" || cfg.Auth.Token == "" {
		return nil
	}
	return []config.ClientTunnel{{ID: "default", Name: "default", Server: cfg.Server.Address, Token: cfg.Auth.Token, Enabled: true}}
}

func sameDefinition(left, right config.ClientTunnel) bool {
	return left.Server == right.Server && left.Token == right.Token
}

func statusMessage(info agent.StatusInfo) string {
	if info.Error != "" {
		return info.Status + ": " + info.Error
	}
	return info.Status
}
