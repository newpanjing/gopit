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
}

// NewAttached 创建客户端附加式 TUI。
func NewAttached(configPath string, logger *slog.Logger) *Model {
	return &Model{configPath: configPath, logger: logger, loader: func() (manager.Snapshot, error) {
		return clientmanagement.ReadSnapshot(clientmanagement.SocketPath(configPath))
	}, width: 90}
}

// SetModeSwitcher 设置切换到服务端模式的持久化回调。
func (m *Model) SetModeSwitcher(switcher func() error) { m.modeSwitcher = switcher }

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
	header := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39")).Render("◉ GoPit Client")
	nav := "  1:隧道    2:状态    3:事件"
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
		content += "\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("⚠ "+m.errMsg)
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
	lines := []string{"隧道 / Tunnels", "  Name                 Server                   Enabled  Status       Streams  Rate"}
	for index, item := range m.snapshot.Tunnels {
		marker := " "
		if index == m.cursor {
			marker = "▸"
		}
		name := item.Name
		if name == "" {
			name = item.ID
		}
		rate := fmt.Sprintf("↑%s/s ↓%s/s", bytes(item.SendRate), bytes(item.RecvRate))
		lines = append(lines, fmt.Sprintf("%s %-20s %-24s %-8t %-12s %-8d %s", marker, trim(name, 20), trim(item.Server, 24), item.Enabled, item.Status.Status, item.Status.ActiveStreams, rate))
	}
	if len(m.snapshot.Tunnels) == 0 {
		lines = append(lines, "  暂无隧道。使用 pit join <token> -s <server> 添加。")
	}
	return box(strings.Join(lines, "\n"))
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
	return box(fmt.Sprintf("状态 / Status\n\nOnline Tunnels    %d\nActive Streams    %d\nBytes Sent        %s\nBytes Received    %s", connected, streams, bytes(sent), bytes(received)))
}

func (m *Model) renderEvents() string {
	lines := []string{"事件 / Events"}
	for _, event := range m.snapshot.Events {
		lines = append(lines, fmt.Sprintf("%s  %-16s %s", event.Time.Format("15:04:05"), trim(event.TunnelID, 16), event.Message))
	}
	if len(m.snapshot.Events) == 0 {
		lines = append(lines, "暂无运行事件")
	}
	return box(strings.Join(lines, "\n"))
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
func box(value string) string {
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2).Render(value)
}
