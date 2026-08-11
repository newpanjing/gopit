// Package tui 提供客户端多隧道后台管理界面。
package tui

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	clientmanagement "gopit/internal/client/management"
	"gopit/internal/client/manager"
	"gopit/internal/config"
)

const refreshInterval = time.Second

type tickMsg time.Time

// Model 是附加到客户端后台管理器的终端界面。
type Model struct {
	configPath   string
	logger       *slog.Logger
	loader       func() (manager.Snapshot, error)
	snapshot     manager.Snapshot
	cursor       int
	page         int
	errMsg       string
	width        int
	modeSwitcher func() error
	appVersion   string
}

// NewAttached 创建客户端附加式 TUI。
func NewAttached(configPath string, logger *slog.Logger) *Model {
	return &Model{configPath: configPath, logger: logger, loader: func() (manager.Snapshot, error) {
		return clientmanagement.ReadSnapshot(clientmanagement.SocketPath(configPath))
	}, width: 90, appVersion: "dev"}
}

// SetModeSwitcher 设置切换到服务端模式的持久化回调。
func (m *Model) SetModeSwitcher(switcher func() error) { m.modeSwitcher = switcher }

// SetVersion 设置显示在客户端界面标题中的程序版本。
func (m *Model) SetVersion(value string) { m.appVersion = value }

func (m *Model) Init() tea.Cmd { return m.refreshCmd() }

func (m *Model) refreshCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return tickMsg(time.Now()) })
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = value.Width
	case tickMsg:
		m.refresh()
		return m, m.refreshCmd()
	case tea.KeyMsg:
		switch value.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "m":
			if m.modeSwitcher != nil {
				if err := m.modeSwitcher(); err != nil {
					m.errMsg = err.Error()
				} else {
					m.errMsg = "已切换为服务端模式；重启后进入服务端界面"
				}
			}
		case "1":
			m.page = 0
		case "2":
			m.page = 1
		case "3":
			m.page = 2
		case "left":
			if m.page > 0 {
				m.page--
			}
		case "right":
			if m.page < 2 {
				m.page++
			}
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.snapshot.Tunnels)-1 {
				m.cursor++
			}
		case " ":
			if m.page == 0 {
				m.toggleSelected()
			}
		case "d":
			if m.page == 0 {
				m.removeSelected()
			}
		}
		m.refresh()
	}
	return m, nil
}

func (m *Model) View() string {
	if m.width == 0 {
		m.width = 90
	}
	header := clientTitleStyle.Render("◉ GoPit Client") + clientVersionStyle.Render("v"+m.appVersion)
	nav := m.renderNavigation()
	var content string
	switch m.page {
	case 0:
		content = m.renderTunnels()
	case 1:
		content = m.renderStatus()
	default:
		content = m.renderEvents()
	}
	if m.errMsg != "" {
		content += "\n" + clientErrorStyle.Render("⚠ "+m.errMsg)
	}
	return header + "\n" + nav + "\n\n" + content + "\n\n" + m.help()
}

func (m *Model) refresh() {
	snapshot, err := m.loader()
	if err != nil {
		m.errMsg = "读取客户端状态失败: " + err.Error()
		return
	}
	m.snapshot = snapshot
	m.errMsg = ""
	if m.cursor >= len(snapshot.Tunnels) {
		m.cursor = max(0, len(snapshot.Tunnels)-1)
	}
}

func (m *Model) renderTunnels() string {
	rows := make([]string, 0, len(m.snapshot.Tunnels)+1)
	rows = append(rows, clientTableHeaderStyle.Render(fmt.Sprintf("%-18s %-22s %-22s %-9s %-12s %-8s %s", "Name", "Forward", "Server", "Enabled", "Status", "Streams", "Rate")))
	for index, item := range m.snapshot.Tunnels {
		name := item.Name
		if name == "" {
			name = "-"
		}
		rate := fmt.Sprintf("↑%s/s ↓%s/s", bytes(item.SendRate), bytes(item.RecvRate))
		enabled := clientDisabledStyle.Render("disabled")
		if item.Enabled {
			enabled = clientEnabledStyle.Render("enabled")
		}
		target := item.Status.Target
		if target == "" {
			target = "-"
		}
		line := fmt.Sprintf("%-18s %-22s %-22s %-9s %-12s %-8d %s", trim(name, 18), trim(target, 22), trim(item.Server, 22), enabled, renderConnectionStatus(item.Status.Status), item.Status.ActiveStreams, clientRateStyle.Render(rate))
		if index == m.cursor {
			line = clientSelectedRowStyle.Render("▸ " + line)
		} else {
			line = "  " + line
		}
		rows = append(rows, line)
	}
	if len(m.snapshot.Tunnels) == 0 {
		rows = append(rows, clientMutedStyle.Render("暂无隧道。使用 pit join <token> -s <server> 添加。"))
	}
	return clientBox("隧道 / Tunnels", strings.Join(rows, "\n"))
}

func (m *Model) renderStatus() string {
	connected := 0
	streams := int32(0)
	var sent, received int64
	for _, item := range m.snapshot.Tunnels {
		if item.Status.Status == "connected" {
			connected++
		}
		streams += item.Status.ActiveStreams
		sent += item.Status.BytesSent
		received += item.Status.BytesReceived
	}
	content := fmt.Sprintf("Online Tunnels    %s\nActive Streams    %s\nBytes Sent        %s\nBytes Received    %s",
		clientMetricStyle.Render(fmt.Sprintf("%d", connected)),
		clientMetricStyle.Render(fmt.Sprintf("%d", streams)),
		clientMetricStyle.Render(bytes(sent)),
		clientMetricStyle.Render(bytes(received)),
	)
	return clientBox("状态 / Status", content)
}

func (m *Model) renderEvents() string {
	lines := make([]string, 0, len(m.snapshot.Events))
	for _, event := range m.snapshot.Events {
		line := fmt.Sprintf("%s  %-16s  %s", event.Time.Format("15:04:05"), trim(event.TunnelID, 16), event.Message)
		lines = append(lines, renderEventLine(line, event.Message))
	}
	if len(m.snapshot.Events) == 0 {
		lines = append(lines, clientMutedStyle.Render("暂无运行事件"))
	}
	return clientBox("事件 / Events", strings.Join(lines, "\n"))
}

// renderNavigation 以选中背景区分当前客户端页面。
func (m *Model) renderNavigation() string {
	labels := []string{"1:隧道", "2:状态", "3:事件"}
	items := make([]string, 0, len(labels))
	for index, label := range labels {
		if index == m.page {
			items = append(items, clientActiveTabStyle.Render(label))
		} else {
			items = append(items, clientInactiveTabStyle.Render(label))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Left, items...)
}

// renderConnectionStatus 根据连接状态突出显示当前可用性。
func renderConnectionStatus(status string) string {
	switch status {
	case "connected":
		return clientConnectedStyle.Render(status)
	case "connecting":
		return clientConnectingStyle.Render(status)
	default:
		return clientDisconnectedStyle.Render(status)
	}
}

// renderEventLine 根据运行事件的严重程度显示颜色。
func renderEventLine(line, message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "failed"), strings.Contains(lower, "error"), strings.Contains(message, "断开"):
		return clientEventErrorStyle.Render(line)
	case strings.Contains(lower, "connecting"), strings.Contains(lower, "change"), strings.Contains(message, "重连"):
		return clientEventWarnStyle.Render(line)
	default:
		return clientEventInfoStyle.Render(line)
	}
}

func (m *Model) help() string {
	if m.page == 0 {
		return "↑↓:选择  space:启动/停止  d:移除  m:服务端模式  ←→:切换页面  q:退出"
	}
	return "←→:切换页面  q:退出"
}

func (m *Model) toggleSelected() {
	m.updateSelected(func(item *config.ClientTunnel) { item.Enabled = !item.Enabled })
}
func (m *Model) removeSelected() {
	if m.cursor < 0 || m.cursor >= len(m.snapshot.Tunnels) {
		return
	}
	id := m.snapshot.Tunnels[m.cursor].ID
	cfg, err := config.LoadClientConfig(m.configPath)
	if err != nil {
		m.errMsg = err.Error()
		return
	}
	items := clientTunnels(cfg)
	result := make([]config.ClientTunnel, 0, len(items))
	for _, item := range items {
		if item.ID != id {
			result = append(result, item)
		}
	}
	cfg.Tunnels = result
	cfg.Server = config.ServerRef{}
	cfg.Auth = config.AuthRef{}
	if err := config.SaveClientConfig(m.configPath, cfg); err != nil {
		m.errMsg = err.Error()
	}
}
func (m *Model) updateSelected(update func(*config.ClientTunnel)) {
	if m.cursor < 0 || m.cursor >= len(m.snapshot.Tunnels) {
		return
	}
	id := m.snapshot.Tunnels[m.cursor].ID
	cfg, err := config.LoadClientConfig(m.configPath)
	if err != nil {
		m.errMsg = err.Error()
		return
	}
	cfg.Tunnels = clientTunnels(cfg)
	cfg.Server = config.ServerRef{}
	cfg.Auth = config.AuthRef{}
	for index := range cfg.Tunnels {
		if cfg.Tunnels[index].ID == id {
			update(&cfg.Tunnels[index])
			break
		}
	}
	if err := config.SaveClientConfig(m.configPath, cfg); err != nil {
		m.errMsg = err.Error()
	}
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
func trim(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length-1] + "…"
}
func bytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%dB", value)
	}
	if value < unit*unit {
		return fmt.Sprintf("%.1fKB", float64(value)/unit)
	}
	return fmt.Sprintf("%.1fMB", float64(value)/(unit*unit))
}

// clientBox 为客户端页面构建带标题的统一信息面板。
func clientBox(title, body string) string {
	return clientBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, clientBoxTitleStyle.Render(title), body))
}

var (
	clientPrimaryColor = lipgloss.Color("#67E8F9")
	clientAccentColor  = lipgloss.Color("#22D3EE")
	clientMutedColor   = lipgloss.Color("#64748B")
	clientPanelColor   = lipgloss.Color("#1E293B")
	clientSelectColor  = lipgloss.Color("#0E7490")

	clientTitleStyle        = lipgloss.NewStyle().Bold(true).Foreground(clientPrimaryColor).Padding(0, 1)
	clientVersionStyle      = lipgloss.NewStyle().Foreground(clientMutedColor).MarginLeft(1)
	clientActiveTabStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#042F2E")).Background(clientAccentColor).Padding(0, 2)
	clientInactiveTabStyle  = lipgloss.NewStyle().Foreground(clientMutedColor).Background(clientPanelColor).Padding(0, 2)
	clientBoxStyle          = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(clientMutedColor).Padding(0, 1).MarginTop(1)
	clientBoxTitleStyle     = lipgloss.NewStyle().Bold(true).Foreground(clientAccentColor).MarginBottom(1)
	clientTableHeaderStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#CBD5E1"))
	clientSelectedRowStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ECFEFF")).Background(clientSelectColor)
	clientEnabledStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#86EFAC"))
	clientDisabledStyle     = lipgloss.NewStyle().Foreground(clientMutedColor)
	clientConnectedStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#86EFAC"))
	clientConnectingStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FDE68A"))
	clientDisconnectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FCA5A5"))
	clientRateStyle         = lipgloss.NewStyle().Foreground(clientPrimaryColor)
	clientMetricStyle       = lipgloss.NewStyle().Bold(true).Foreground(clientPrimaryColor)
	clientMutedStyle        = lipgloss.NewStyle().Foreground(clientMutedColor)
	clientEventInfoStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#A7F3D0"))
	clientEventWarnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FDE68A"))
	clientEventErrorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FCA5A5"))
	clientErrorStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#F87171")).Bold(true)
)
