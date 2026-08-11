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

const refreshInterval = 500 * time.Millisecond

const githubRepositoryURL = "github.com/newpanjing/gopit"

const (
	clientMarkerWidth  = 2
	clientNameWidth    = 18
	clientForwardWidth = 22
	clientServerWidth  = 22
	clientEnabledWidth = 9
	clientStatusWidth  = 12
	clientStreamsWidth = 8
	clientLogRowCount  = 12
)

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
	header := clientTitleStyle.Render("◉ GoPit Client") + clientVersionStyle.Render("v"+strings.TrimPrefix(m.appVersion, "v"))
	repository := clientGitHubStyle.Render(githubRepositoryURL)
	nav := m.renderNavigation()
	var content string
	switch m.page {
	case 0:
		content = m.renderTunnels()
	case 1:
		content = m.renderStatus()
	default:
		content = m.renderLogs()
	}
	if m.errMsg != "" {
		content += "\n" + clientErrorStyle.Render("⚠ "+m.errMsg)
	}
	return header + "\n" + repository + "\n" + nav + "\n\n" + content + "\n\n" + m.help()
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
	rows = append(rows, clientTableHeaderStyle.Render(renderTunnelRow("", "Name", "Forward", "Server", "Enabled", "Status", "Streams", "Rate")))
	for index, item := range m.snapshot.Tunnels {
		name := item.Name
		if name == "" {
			name = "-"
		}
		rate := fmt.Sprintf("↑%s/s ↓%s/s", bytes(item.SendRate), bytes(item.RecvRate))
		enabled := clientDisabledStyle.Render(fixedClientCell("✗", clientEnabledWidth))
		if item.Enabled {
			enabled = clientEnabledStyle.Render(fixedClientCell("✓", clientEnabledWidth))
		}
		target := item.Status.Target
		if target == "" {
			target = "-"
		}
		status := renderConnectionStatus(item.Status.Status)
		line := renderTunnelRow("", trim(name, clientNameWidth), trim(target, clientForwardWidth), trim(item.Server, clientServerWidth), enabled, status, fmt.Sprintf("%d", item.Status.ActiveStreams), clientRateStyle.Render(rate))
		if index == m.cursor {
			line = clientSelectedRowStyle.Render(renderTunnelRow("▸", line))
		} else {
			line = renderTunnelRow("", line)
		}
		rows = append(rows, line)
	}
	if len(m.snapshot.Tunnels) == 0 {
		rows = append(rows, clientMutedStyle.Render("暂无隧道。使用 pit join <token> -s <server> 添加。"))
	}
	return clientBox("隧道 / Tunnels", strings.Join(rows, "\n"))
}

// renderTunnelRow 使用固定列宽拼接客户端隧道表格，避免 ANSI 颜色码干扰 fmt 的对齐计算。
func renderTunnelRow(marker string, values ...string) string {
	if len(values) == 1 {
		return fixedClientCell(marker, clientMarkerWidth) + values[0]
	}
	widths := []int{clientNameWidth, clientForwardWidth, clientServerWidth, clientEnabledWidth, clientStatusWidth, clientStreamsWidth}
	parts := make([]string, 0, len(values)+1)
	parts = append(parts, fixedClientCell(marker, clientMarkerWidth))
	for index, value := range values {
		if index < len(widths) {
			parts = append(parts, fixedClientCell(value, widths[index]))
			continue
		}
		parts = append(parts, value)
	}
	return strings.Join(parts, " ")
}

// fixedClientCell 按终端显示宽度补齐单元格，调用方应在着色前完成补齐。
func fixedClientCell(value string, width int) string {
	padding := width - lipgloss.Width(value)
	if padding <= 0 {
		return value
	}
	return value + strings.Repeat(" ", padding)
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

// renderLogs 仅渲染客户端转发完成的请求记录，避免状态事件干扰请求日志。
func (m *Model) renderLogs() string {
	requestRows := make([]string, 0, clientLogRowCount-1)
	for _, event := range m.snapshot.Events {
		if event.Method == "" {
			continue
		}
		requestRows = append(requestRows, renderClientLogRow(
			clientLogTimeStyle.Render(fixedClientCell(event.Time.Format("15:04:05"), 10)),
			clientLogMethodStyle.Render(fixedClientCell(event.Method, 8)),
			clientLogAddressStyle.Render(fixedClientCell(trim(event.Address, 28), 28)),
			clientLogTargetStyle.Render(fixedClientCell(trim(event.Target, 24), 24)),
			renderClientDuration(event.Duration),
		))
	}
	if len(requestRows) > clientLogRowCount-1 {
		requestRows = requestRows[len(requestRows)-(clientLogRowCount-1):]
	}
	rows := []string{clientTableHeaderStyle.Render(renderClientLogRow("Time", "Method", "Address", "Forward", "Duration"))}
	if len(requestRows) == 0 {
		rows = append(rows, clientMutedStyle.Render("暂无请求日志"))
	}
	rows = append(rows, requestRows...)
	for len(rows) < clientLogRowCount {
		rows = append(rows, "")
	}
	return clientBox("日志 / Logs", strings.Join(rows, "\n"))
}

// renderClientLogRow 使用固定列宽渲染客户端请求日志。
func renderClientLogRow(values ...string) string {
	widths := []int{10, 8, 28, 24}
	parts := make([]string, 0, len(values))
	for index, value := range values {
		if index < len(widths) {
			parts = append(parts, fixedClientCell(value, widths[index]))
			continue
		}
		parts = append(parts, value)
	}
	return strings.Join(parts, " ")
}

// renderClientDuration 根据响应耗时突出显示异常缓慢的请求。
func renderClientDuration(duration time.Duration) string {
	value := duration.Round(time.Millisecond).String()
	if duration >= time.Second {
		return clientLogSlowStyle.Render(value)
	}
	return clientLogDurationStyle.Render(value)
}

// renderNavigation 以选中背景区分当前客户端页面。
func (m *Model) renderNavigation() string {
	labels := []string{"1:隧道", "2:状态", "3:日志"}
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

// renderConnectionStatus 使用状态符号突出显示当前客户端连接可用性。
func renderConnectionStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "connected":
		return clientConnectedStyle.Render(fixedClientCell("✓", clientStatusWidth))
	case "connecting":
		return clientConnectingStyle.Render(fixedClientCell("…", clientStatusWidth))
	default:
		return clientDisconnectedStyle.Render(fixedClientCell("✗", clientStatusWidth))
	}
}

// renderEventLine 根据运行事件的严重程度显示颜色。
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
	clientDisabledStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#FCA5A5"))
	clientConnectedStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#86EFAC"))
	clientConnectingStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FDE68A"))
	clientDisconnectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FCA5A5"))
	clientRateStyle         = lipgloss.NewStyle().Foreground(clientPrimaryColor)
	clientMetricStyle       = lipgloss.NewStyle().Bold(true).Foreground(clientPrimaryColor)
	clientMutedStyle        = lipgloss.NewStyle().Foreground(clientMutedColor)
	clientLogTimeStyle      = lipgloss.NewStyle().Foreground(clientMutedColor)
	clientLogMethodStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#86EFAC"))
	clientLogAddressStyle   = lipgloss.NewStyle().Foreground(clientPrimaryColor)
	clientLogTargetStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#C4B5FD"))
	clientLogDurationStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#A7F3D0"))
	clientLogSlowStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FDE68A"))
	clientGitHubStyle       = lipgloss.NewStyle().Foreground(clientMutedColor)
	clientErrorStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#F87171")).Bold(true)
)
