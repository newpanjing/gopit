// Package tui implements an interactive terminal UI for managing the GoPit
// server: tunnel connections, live status, logs and server settings.
//
// It is built with Bubble Tea (github.com/charmbracelet/bubbletea) and uses
// bubbles/textinput for inline form entry and lipgloss for styling.
package tui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"gopit/internal/config"
	"gopit/internal/server/app"
	"gopit/internal/server/auth"
	"gopit/internal/server/configstore"
)

// --- pages & modes -------------------------------------------------------

type page int

const (
	pageConnections page = iota
	pageStatus
	pageLogs
	pageSettings
	numPages
)

var pageNames = [numPages]string{"隧道", "状态", "日志", "设置"}

const githubRepositoryURL = "github.com/newpanjing/gopit"
const serverLogRowCount = 12

type mode int

const (
	modeNormal mode = iota
	modeInput
	modeConfirm
	modeToken
)

type inputKind int

const (
	inputNewConn inputKind = iota
	inputEditConn
	inputToken
	inputSettings
)

// AttachedStatusLoader 读取后台服务实时状态，供附加式 TUI 刷新显示。
type AttachedStatusLoader func() (app.Stats, []app.OnlineClientInfo, error)

var connectionTypes = []string{
	config.ConnectionTypeHTTP,
	config.ConnectionTypeTCP,
	config.ConnectionTypeUDP,
}

type deleteKind int

const (
	deleteConn deleteKind = iota
)

// --- async result messages ----------------------------------------------

type tickMsg time.Time

type createConnResult struct {
	connID string
	token  string
	err    error
}

type tokenResult struct {
	connID string
	token  string
	err    error
}

type opResult struct {
	err error
}

// tokenInfo 承载可重复查看的连接 Token。
type tokenInfo struct {
	connID  string
	token   string
	rotated bool
}

// --- Model --------------------------------------------------------------

// Model is the Bubble Tea model for the server TUI.
type Model struct {
	app          *app.App
	store        *configstore.Store
	logger       *slog.Logger
	attached     bool
	statusLoader AttachedStatusLoader
	modeSwitcher func() error
	logPath      string
	appVersion   string

	page   page
	cursor map[page]int

	// modal state
	mode        mode
	inputKind   inputKind
	inputs      []textinput.Model
	inputIdx    int
	protocolIdx int
	inputLabels []string
	inputTitle  string
	editID      string

	confirmKind deleteKind
	confirmID   string
	confirmMsg  string

	token tokenInfo

	errMsg string

	// status detail toggle
	showDetail bool

	// cached view data
	stats         app.Stats
	onlineClients []app.OnlineClientInfo

	// log buffer (max 100 lines)
	logLines []string

	width  int
	height int

	quitting bool
}

// New creates a new TUI model bound to the given app and config store.
func New(a *app.App, store *configstore.Store, logger *slog.Logger) *Model {
	m := &Model{
		app:        a,
		store:      store,
		logger:     logger,
		page:       pageConnections,
		cursor:     make(map[page]int, numPages),
		width:      80,
		height:     24,
		showDetail: true,
		logLines:   make([]string, 0, 100),
		appVersion: "dev",
	}
	for p := page(0); p < numPages; p++ {
		m.cursor[p] = 0
	}
	m.refresh()
	return m
}

// NewAttached 创建连接到后台服务配置文件的管理界面。
// 后台服务会通过配置热加载应用此界面的修改。
func NewAttached(store *configstore.Store, logger *slog.Logger, statusLoader AttachedStatusLoader) *Model {
	m := &Model{
		store:        store,
		logger:       logger,
		attached:     true,
		statusLoader: statusLoader,
		page:         pageConnections,
		cursor:       make(map[page]int, numPages),
		width:        80,
		height:       24,
		showDetail:   true,
		logLines:     make([]string, 0, 100),
		appVersion:   "dev",
	}
	for p := page(0); p < numPages; p++ {
		m.cursor[p] = 0
	}
	m.refresh()
	return m
}

// SetModeSwitcher 设置切换到客户端模式的持久化回调，由命令层决定配置路径。
func (m *Model) SetModeSwitcher(switcher func() error) { m.modeSwitcher = switcher }

// SetVersion 设置显示在终端界面标题中的程序版本。
func (m *Model) SetVersion(value string) { m.appVersion = value }

// SetLogPath 设置后台日志文件路径，刷新时会读取最近日志用于高亮展示。
func (m *Model) SetLogPath(path string) { m.logPath = path }

// LogLine appends a single log line to the model's log buffer, trimming to
// the most recent 100 lines.
func (m *Model) LogLine(line string) {
	m.logLines = append(m.logLines, line)
	if len(m.logLines) > 100 {
		m.logLines = m.logLines[len(m.logLines)-100:]
	}
}

// Init starts the auto-refresh ticker.
func (m *Model) Init() tea.Cmd {
	return tick()
}

// Update handles all messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		// Global quit.
		if msg.String() == "ctrl+c" {
			m.quitting = true
			return m, tea.Quit
		}
		switch m.mode {
		case modeToken:
			m.mode = modeNormal
			m.token = tokenInfo{}
			return m, nil
		case modeInput:
			return m.handleInputKey(msg)
		case modeConfirm:
			return m.handleConfirmKey(msg)
		default:
			return m.handleNormalKey(msg)
		}

	case tickMsg:
		m.refresh()
		return m, tick()

	case createConnResult:
		if msg.err != nil {
			m.errMsg = "create connection failed: " + msg.err.Error()
			m.mode = modeNormal
		} else {
			m.mode = modeToken
			m.token = tokenInfo{connID: msg.connID, token: msg.token, rotated: false}
		}
		m.refresh()
		return m, nil

	case tokenResult:
		if msg.err != nil {
			m.errMsg = "update token failed: " + msg.err.Error()
			m.mode = modeNormal
		} else {
			m.mode = modeToken
			m.token = tokenInfo{connID: msg.connID, token: msg.token, rotated: true}
		}
		m.refresh()
		return m, nil

	case opResult:
		if msg.err != nil {
			m.errMsg = msg.err.Error()
		}
		m.refresh()
		return m, nil

	default:
		// Forward non-key messages (e.g. cursor blink) to the active input.
		if m.mode == modeInput && len(m.inputs) > 0 {
			var cmd tea.Cmd
			m.inputs[m.inputIdx], cmd = m.inputs[m.inputIdx].Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

// View renders the current screen.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}

	title := m.renderTitle()
	status := m.renderStatus()

	var middle string
	switch m.mode {
	case modeInput:
		middle = m.renderInputModal()
	case modeToken:
		middle = m.renderTokenModal()
	default:
		content := m.renderPage()
		middle = content
		if m.mode == modeConfirm {
			middle = lipgloss.JoinVertical(lipgloss.Left, content, "", m.renderConfirmModal())
		}
	}

	parts := []string{title, middle, status}
	if m.errMsg != "" {
		parts = append(parts, m.renderError())
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// --- key handling -------------------------------------------------------

func (m *Model) handleNormalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Clear any stale error on the next key press.
	m.errMsg = ""

	switch msg.String() {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "m":
		if m.modeSwitcher != nil {
			if err := m.modeSwitcher(); err != nil {
				m.errMsg = err.Error()
			} else {
				m.errMsg = "已切换为客户端模式；重启后进入客户端界面"
			}
		}
		return m, nil
	case "tab":
		m.setPage((m.page + 1) % numPages)
		return m, nil
	case "shift+tab":
		m.setPage((m.page - 1 + numPages) % numPages)
		return m, nil
	case "left":
		m.setPage((m.page - 1 + numPages) % numPages)
		return m, nil
	case "right":
		m.setPage((m.page + 1) % numPages)
		return m, nil
	case "1":
		m.setPage(pageConnections)
		return m, nil
	case "2":
		m.setPage(pageStatus)
		return m, nil
	case "3":
		m.setPage(pageLogs)
		return m, nil
	case "4":
		m.setPage(pageSettings)
		return m, nil
	case "up", "k":
		m.moveCursor(-1)
		return m, nil
	case "down", "j":
		m.moveCursor(1)
		return m, nil
	case "esc":
		return m, nil
	}

	switch m.page {
	case pageConnections:
		return m.handleConnectionsKey(msg)
	case pageStatus:
		return m.handleStatusKey(msg)
	case pageSettings:
		return m.handleSettingsKey(msg)
	}
	return m, nil
}

// setPage 切换页面，并且仅在进入日志页时读取后台日志文件。
func (m *Model) setPage(value page) {
	m.page = value
	if value == pageLogs {
		m.refreshLogLines()
	}
}

func (m *Model) handleConnectionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	c := m.selectedConnection()
	switch msg.String() {
	case "a":
		return m.startInput(inputNewConn)
	case "e":
		if c == nil {
			return m, nil
		}
		return m.startInput(inputEditConn)
	case "t":
		if c == nil {
			return m, nil
		}
		if c.Token == "" {
			m.errMsg = "该旧连接未保存明文 Token，请按 r 重置后再查看"
			return m, nil
		}
		m.mode = modeToken
		m.token = tokenInfo{connID: c.ID, token: c.Token}
		return m, nil
	case "r":
		if c == nil {
			return m, nil
		}
		return m.startInput(inputToken)
	case "d":
		if c == nil {
			return m, nil
		}
		m.mode = modeConfirm
		m.confirmKind = deleteConn
		m.confirmID = c.ID
		m.confirmMsg = fmt.Sprintf("Delete connection %q (%s)?", c.Name, c.ID)
		return m, nil
	case " ":
		if c == nil {
			return m, nil
		}
		return m, m.toggleConnection(c.ID, c.Enabled)
	}
	return m, nil
}

func (m *Model) handleStatusKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.showDetail = !m.showDetail
		return m, nil
	}
	return m, nil
}

func (m *Model) handleSettingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "e":
		return m.startInput(inputSettings)
	}
	return m, nil
}

func (m *Model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// 协议字段是受控选择器，只允许在固定协议列表中切换。
	if m.inputIdx == 0 && m.inputKind != inputToken && m.inputKind != inputSettings {
		switch msg.String() {
		case "left":
			m.protocolIdx = (m.protocolIdx + len(connectionTypes) - 1) % len(connectionTypes)
			m.inputs[0].SetValue(connectionTypes[m.protocolIdx])
			return m, nil
		case "right", " ":
			m.protocolIdx = (m.protocolIdx + 1) % len(connectionTypes)
			m.inputs[0].SetValue(connectionTypes[m.protocolIdx])
			return m, nil
		}
	}
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.editID = ""
		return m, nil
	case "tab":
		m.inputs[m.inputIdx].Blur()
		m.inputIdx = (m.inputIdx + 1) % len(m.inputs)
		return m, m.inputs[m.inputIdx].Focus()
	case "shift+tab":
		m.inputs[m.inputIdx].Blur()
		m.inputIdx = (m.inputIdx - 1 + len(m.inputs)) % len(m.inputs)
		return m, m.inputs[m.inputIdx].Focus()
	case "up":
		m.inputs[m.inputIdx].Blur()
		m.inputIdx = (m.inputIdx - 1 + len(m.inputs)) % len(m.inputs)
		return m, m.inputs[m.inputIdx].Focus()
	case "down":
		m.inputs[m.inputIdx].Blur()
		m.inputIdx = (m.inputIdx + 1) % len(m.inputs)
		return m, m.inputs[m.inputIdx].Focus()
	case "enter":
		return m.submitInput()
	}
	if m.inputIdx == 0 && m.inputKind != inputToken && m.inputKind != inputSettings {
		return m, nil
	}
	var cmd tea.Cmd
	m.inputs[m.inputIdx], cmd = m.inputs[m.inputIdx].Update(msg)
	return m, cmd
}

func (m *Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		kind := m.confirmKind
		id := m.confirmID
		m.mode = modeNormal
		switch kind {
		case deleteConn:
			return m, m.deleteConnection(id)
		}
	case "n", "N", "esc":
		m.mode = modeNormal
		return m, nil
	}
	return m, nil
}

// --- input helpers ------------------------------------------------------

func (m *Model) startInput(kind inputKind) (tea.Model, tea.Cmd) {
	var inputs []textinput.Model
	var labels []string
	var title string

	switch kind {
	case inputNewConn:
		typ := newTextInput(config.ConnectionTypeTCP)
		typ.SetValue(config.ConnectionTypeTCP)
		name := newTextInput("my-connection")
		host := newTextInput("(empty=any, http only)")
		port := newTextInput("9000 (HTTP: 0=default Web port)")
		target := newTextInput("localhost:8080")
		token := newTextInput("(empty=auto-generate)")
		forwardHost := newTextInput("false")
		inputs = []textinput.Model{typ, name, host, port, target, token, forwardHost}
		labels = []string{"Protocol (HTTP/TCP/UDP)", "Name", "Host", "Listen Port (HTTP: 0=default Web)", "Target", "Token", "Forward Host (HTTP only)"}
		title = "新建连接 / Create Connection"
	case inputEditConn:
		c := m.selectedConnection()
		if c == nil {
			return m, nil
		}
		typ := newTextInput(config.ConnectionTypeTCP)
		connType := c.Type
		if connType == "" {
			connType = config.ConnectionTypeTCP
		}
		typ.SetValue(connType)
		name := newTextInput("my-connection")
		name.SetValue(c.Name)
		host := newTextInput("(empty=any, http only)")
		host.SetValue(c.Host)
		port := newTextInput("9000 (HTTP: 0=default Web port)")
		port.SetValue(strconv.Itoa(c.ListenPort))
		target := newTextInput("localhost:8080")
		target.SetValue(c.Target)
		forwardHost := newTextInput("false")
		forwardHost.SetValue(strconv.FormatBool(c.ForwardHost))
		inputs = []textinput.Model{typ, name, host, port, target, forwardHost}
		labels = []string{"Protocol (HTTP/TCP/UDP)", "Name", "Host", "Listen Port (HTTP: 0=default Web)", "Target", "Forward Host (HTTP only)"}
		title = fmt.Sprintf("编辑连接 / Edit Connection (%s)", c.ID)
		m.editID = c.ID
	case inputToken:
		c := m.selectedConnection()
		if c == nil {
			return m, nil
		}
		token := newTextInput("(empty=auto-generate)")
		inputs = []textinput.Model{token}
		labels = []string{"New Token"}
		title = fmt.Sprintf("重置 Token / Reset Token (%s)", c.ID)
		m.editID = c.ID
	case inputSettings:
		cfg := m.store.Get()
		tunnelListen := newTextInput(":7001")
		tunnelListen.SetValue(cfg.Server.TunnelListen)
		httpListen := newTextInput(":7777")
		httpListen.SetValue(cfg.Server.HTTPListen)
		httpsListen := newTextInput(":443")
		httpsListen.SetValue(cfg.Server.HTTPSListen)
		inputs = []textinput.Model{tunnelListen, httpListen, httpsListen}
		labels = []string{"Tunnel Listen", "Web HTTP Listen", "Web HTTPS Listen"}
		title = "编辑监听地址 / Edit Listen Addresses"
	}
	m.inputs = inputs
	m.inputLabels = labels
	m.inputTitle = title
	m.inputKind = kind
	m.inputIdx = 0
	if kind == inputNewConn || kind == inputEditConn {
		m.protocolIdx = protocolIndex(inputs[0].Value())
		m.inputs[0].SetValue(connectionTypes[m.protocolIdx])
	}
	m.mode = modeInput
	return m, inputs[0].Focus()
}

func (m *Model) submitInput() (tea.Model, tea.Cmd) {
	switch m.inputKind {
	case inputNewConn:
		// fields: Type, Name, Host, ListenPort, Target, ForwardHost, Token
		connType := strings.TrimSpace(strings.ToLower(m.inputs[0].Value()))
		name := strings.TrimSpace(m.inputs[1].Value())
		host := strings.TrimSpace(m.inputs[2].Value())
		portStr := strings.TrimSpace(m.inputs[3].Value())
		target := strings.TrimSpace(m.inputs[4].Value())
		forwardHost, err := parseForwardHost(m.inputs[6].Value())
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		token := strings.TrimSpace(m.inputs[5].Value())

		if connType == "" {
			connType = config.ConnectionTypeTCP
		}
		if !isConnectionType(connType) {
			m.errMsg = "protocol must be http, tcp or udp"
			return m, nil
		}
		if connType != config.ConnectionTypeHTTP {
			forwardHost = false
		}
		if name == "" {
			m.errMsg = "name cannot be empty"
			return m, nil
		}
		if target == "" {
			m.errMsg = "target cannot be empty"
			return m, nil
		}

		port := 0
		if connType == config.ConnectionTypeTCP || connType == config.ConnectionTypeUDP {
			var err error
			port, err = strconv.Atoi(portStr)
			if err != nil || port <= 0 || port > 65535 {
				m.errMsg = "invalid listen port (1-65535)"
				return m, nil
			}
		} else if portStr != "" {
			var err error
			port, err = strconv.Atoi(portStr)
			if err != nil || port < 0 || port > 65535 {
				m.errMsg = "invalid listen port (0-65535)"
				return m, nil
			}
		}
		m.mode = modeNormal
		return m, m.createConnection(name, connType, host, port, target, forwardHost, token)

	case inputEditConn:
		// fields: Type, Name, Host, ListenPort, Target
		connType := strings.TrimSpace(strings.ToLower(m.inputs[0].Value()))
		name := strings.TrimSpace(m.inputs[1].Value())
		host := strings.TrimSpace(m.inputs[2].Value())
		portStr := strings.TrimSpace(m.inputs[3].Value())
		target := strings.TrimSpace(m.inputs[4].Value())
		forwardHost, err := parseForwardHost(m.inputs[5].Value())
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}

		if connType == "" {
			connType = config.ConnectionTypeTCP
		}
		if !isConnectionType(connType) {
			m.errMsg = "protocol must be http, tcp or udp"
			return m, nil
		}
		if connType != config.ConnectionTypeHTTP {
			forwardHost = false
		}
		if name == "" {
			m.errMsg = "name cannot be empty"
			return m, nil
		}
		if target == "" {
			m.errMsg = "target cannot be empty"
			return m, nil
		}

		port := 0
		if connType == config.ConnectionTypeTCP || connType == config.ConnectionTypeUDP {
			var err error
			port, err = strconv.Atoi(portStr)
			if err != nil || port <= 0 || port > 65535 {
				m.errMsg = "invalid listen port (1-65535)"
				return m, nil
			}
		} else if portStr != "" {
			var err error
			port, err = strconv.Atoi(portStr)
			if err != nil || port < 0 || port > 65535 {
				m.errMsg = "invalid listen port (0-65535)"
				return m, nil
			}
		}
		id := m.editID
		m.mode = modeNormal
		m.editID = ""
		return m, m.updateConnection(id, name, connType, host, port, target, forwardHost)

	case inputToken:
		token := strings.TrimSpace(m.inputs[0].Value())
		id := m.editID
		m.mode = modeNormal
		m.editID = ""
		return m, m.updateConnectionToken(id, token)

	case inputSettings:
		tunnelListen := strings.TrimSpace(m.inputs[0].Value())
		httpListen := strings.TrimSpace(m.inputs[1].Value())
		httpsListen := strings.TrimSpace(m.inputs[2].Value())
		for _, listenAddress := range []string{tunnelListen, httpListen, httpsListen} {
			if err := validateListenAddress(listenAddress); err != nil {
				m.errMsg = err.Error()
				return m, nil
			}
		}
		m.mode = modeNormal
		return m, m.updateServerConfig(tunnelListen, httpListen, httpsListen)
	}

	m.mode = modeNormal
	m.editID = ""
	return m, nil
}

// --- commands -----------------------------------------------------------

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// parseForwardHost 解析 HTTP Host 转发开关，避免配置中出现含义不明确的值。
func parseForwardHost(value string) (bool, error) {
	if strings.TrimSpace(value) == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("forward host must be true or false")
	}
	return parsed, nil
}

func (m *Model) createConnection(name, connType, host string, port int, target string, forwardHost bool, customToken string) tea.Cmd {
	if m.attached {
		return m.createAttachedConnection(name, connType, host, port, target, forwardHost, customToken)
	}
	a := m.app
	return func() tea.Msg {
		id, token, err := a.CreateConnection(name, connType, host, port, target, forwardHost, customToken)
		return createConnResult{connID: id, token: token, err: err}
	}
}

func (m *Model) updateConnection(connID, name, connType, host string, port int, target string, forwardHost bool) tea.Cmd {
	if m.attached {
		return m.updateAttachedConnection(connID, name, connType, host, port, target, forwardHost)
	}
	a := m.app
	return func() tea.Msg {
		return opResult{err: a.UpdateConnection(connID, name, connType, host, port, target, forwardHost)}
	}
}

func (m *Model) updateConnectionToken(connID, customToken string) tea.Cmd {
	if m.attached {
		return m.updateAttachedToken(connID, customToken)
	}
	a := m.app
	return func() tea.Msg {
		token, err := a.UpdateConnectionToken(connID, customToken)
		return tokenResult{connID: connID, token: token, err: err}
	}
}

func (m *Model) toggleConnection(connID string, enabled bool) tea.Cmd {
	if m.attached {
		return m.toggleAttachedConnection(connID, enabled)
	}
	a := m.app
	next := !enabled
	return func() tea.Msg {
		return opResult{err: a.SetConnectionEnabled(connID, next)}
	}
}

func (m *Model) deleteConnection(connID string) tea.Cmd {
	if m.attached {
		return m.deleteAttachedConnection(connID)
	}
	a := m.app
	return func() tea.Msg {
		return opResult{err: a.DeleteConnection(connID)}
	}
}

func (m *Model) updateServerConfig(tunnelListen, httpListen, httpsListen string) tea.Cmd {
	if m.attached {
		return m.updateAttachedServerConfig(tunnelListen, httpListen, httpsListen)
	}
	a := m.app
	return func() tea.Msg {
		return opResult{err: a.UpdateServerConfig(tunnelListen, httpListen, httpsListen)}
	}
}

// createAttachedConnection 将新增连接写入后台服务正在监听的配置文件。
func (m *Model) createAttachedConnection(name, connType, host string, port int, target string, forwardHost bool, customToken string) tea.Cmd {
	store := m.store
	return func() tea.Msg {
		connID, err := auth.GenerateClientID()
		if err != nil {
			return createConnResult{err: err}
		}
		token := customToken
		if token == "" {
			token, err = auth.GenerateToken()
			if err != nil {
				return createConnResult{err: err}
			}
		}
		tokenHash, err := auth.HashToken(token)
		if err != nil {
			return createConnResult{err: err}
		}
		err = store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
			cfg.Connections = append(cfg.Connections, config.Connection{
				ID:          connID,
				Name:        name,
				Type:        connType,
				Host:        host,
				ForwardHost: forwardHost,
				ListenPort:  port,
				Target:      target,
				Token:       token,
				TokenHash:   tokenHash,
				Enabled:     true,
				CreatedAt:   time.Now().Format(time.RFC3339),
			})
			return cfg, nil
		})
		return createConnResult{connID: connID, token: token, err: err}
	}
}

// updateAttachedConnection 更新后台服务热加载的连接配置。
func (m *Model) updateAttachedConnection(connID, name, connType, host string, port int, target string, forwardHost bool) tea.Cmd {
	store := m.store
	return func() tea.Msg {
		err := store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
			for i := range cfg.Connections {
				if cfg.Connections[i].ID == connID {
					cfg.Connections[i].Name = name
					cfg.Connections[i].Type = connType
					cfg.Connections[i].Host = host
					cfg.Connections[i].ForwardHost = forwardHost
					cfg.Connections[i].ListenPort = port
					cfg.Connections[i].Target = target
					return cfg, nil
				}
			}
			return nil, fmt.Errorf("connection not found: %s", connID)
		})
		return opResult{err: err}
	}
}

// updateAttachedToken 更新令牌哈希，新的鉴权结果将在后台服务热加载后生效。
func (m *Model) updateAttachedToken(connID, customToken string) tea.Cmd {
	store := m.store
	return func() tea.Msg {
		token := customToken
		var err error
		if token == "" {
			token, err = auth.GenerateToken()
			if err != nil {
				return tokenResult{connID: connID, err: err}
			}
		}
		tokenHash, err := auth.HashToken(token)
		if err != nil {
			return tokenResult{connID: connID, err: err}
		}
		err = store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
			for i := range cfg.Connections {
				if cfg.Connections[i].ID == connID {
					cfg.Connections[i].Token = token
					cfg.Connections[i].TokenHash = tokenHash
					return cfg, nil
				}
			}
			return nil, fmt.Errorf("connection not found: %s", connID)
		})
		return tokenResult{connID: connID, token: token, err: err}
	}
}

// toggleAttachedConnection 更新连接启用状态。
func (m *Model) toggleAttachedConnection(connID string, enabled bool) tea.Cmd {
	store := m.store
	return func() tea.Msg {
		err := store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
			for i := range cfg.Connections {
				if cfg.Connections[i].ID == connID {
					cfg.Connections[i].Enabled = !enabled
					return cfg, nil
				}
			}
			return nil, fmt.Errorf("connection not found: %s", connID)
		})
		return opResult{err: err}
	}
}

// deleteAttachedConnection 从后台服务热加载的配置中删除连接。
func (m *Model) deleteAttachedConnection(connID string) tea.Cmd {
	store := m.store
	return func() tea.Msg {
		err := store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
			connections := cfg.Connections[:0]
			found := false
			for _, conn := range cfg.Connections {
				if conn.ID == connID {
					found = true
					continue
				}
				connections = append(connections, conn)
			}
			if !found {
				return nil, fmt.Errorf("connection not found: %s", connID)
			}
			cfg.Connections = connections
			return cfg, nil
		})
		return opResult{err: err}
	}
}

// updateAttachedServerConfig 更新后台服务热加载的监听地址配置。
func (m *Model) updateAttachedServerConfig(tunnelListen, httpListen, httpsListen string) tea.Cmd {
	store := m.store
	return func() tea.Msg {
		err := store.Update(func(cfg *config.ServerConfig) (*config.ServerConfig, error) {
			cfg.Server.TunnelListen = tunnelListen
			cfg.Server.HTTPListen = httpListen
			cfg.Server.HTTPSListen = httpsListen
			return cfg, nil
		})
		return opResult{err: err}
	}
}

// --- selection & data ---------------------------------------------------

func (m *Model) moveCursor(delta int) {
	n := m.listLen(m.page)
	if n == 0 {
		m.cursor[m.page] = 0
		return
	}
	c := m.cursor[m.page] + delta
	if c < 0 {
		c = 0
	}
	if c >= n {
		c = n - 1
	}
	m.cursor[m.page] = c
}

func (m *Model) listLen(p page) int {
	switch p {
	case pageConnections:
		return len(m.store.Get().Connections)
	case pageStatus:
		return len(m.onlineClients)
	}
	return 0
}

func (m *Model) selectedConnection() *config.Connection {
	cfg := m.store.Get()
	i := m.cursor[pageConnections]
	if i < 0 || i >= len(cfg.Connections) {
		return nil
	}
	c := cfg.Connections[i]
	return &c
}

// isConnectionOnline reports whether the given connection id has an online client.
func (m *Model) isConnectionOnline(connID string) bool {
	for _, c := range m.onlineClients {
		if c.ConnectionID == connID {
			return true
		}
	}
	return false
}

func (m *Model) refresh() {
	if m.page == pageLogs {
		m.refreshLogLines()
	}
	if !m.attached {
		m.stats = m.app.GetStats()
		m.onlineClients = m.app.GetOnlineClients()
	} else if m.statusLoader != nil {
		stats, onlineClients, err := m.statusLoader()
		if err == nil {
			m.stats = stats
			m.onlineClients = onlineClients
		}
	}
	for p := page(0); p < numPages; p++ {
		n := m.listLen(p)
		if n == 0 {
			m.cursor[p] = 0
			continue
		}
		if m.cursor[p] >= n {
			m.cursor[p] = n - 1
		}
		if m.cursor[p] < 0 {
			m.cursor[p] = 0
		}
	}
}

// --- rendering ----------------------------------------------------------

func (m *Model) renderTitle() string {
	title := titleStyle.Render("◉ GoPit Server") + versionStyle.Render("v"+strings.TrimPrefix(m.appVersion, "v"))

	var tabs []string
	for i, name := range pageNames {
		label := fmt.Sprintf("%d:%s", i+1, name)
		if page(i) == m.page {
			tabs = append(tabs, activeTabStyle.Render(label))
		} else {
			tabs = append(tabs, inactiveTabStyle.Render(label))
		}
	}
	tabsRow := lipgloss.JoinHorizontal(lipgloss.Bottom, tabs...)
	return lipgloss.JoinVertical(lipgloss.Left, title, githubStyle.Render(githubRepositoryURL), tabsRow, "")
}

func (m *Model) renderStatus() string {
	hints := "←→:切换页面  Tab/Shift+Tab:切换  1-4:跳转  ↑↓/jk:导航  q:退出"
	switch m.page {
	case pageConnections:
		hints = "a:新建隧道  e:编辑  t:查看Token  r:重置Token  d:删除  space:启用/禁用  m:客户端模式  |  " + hints
	case pageStatus:
		hints = "↑↓:选择连接  Enter:切换详情  |  " + hints
	case pageSettings:
		hints = "e:编辑  |  " + hints
	}
	return statusBarStyle.Render(hints)
}

func (m *Model) renderError() string {
	return errorStyle.Render("⚠ " + m.errMsg)
}

func (m *Model) renderPage() string {
	switch m.page {
	case pageConnections:
		return m.renderConnections()
	case pageStatus:
		return m.renderStatusPage()
	case pageLogs:
		return m.renderLogs()
	case pageSettings:
		return m.renderSettings()
	}
	return ""
}

func (m *Model) renderConnections() string {
	cfg := m.store.Get()
	httpsListen := "disabled"
	if cfg.TLS.Enabled {
		httpsListen = formatListenAddress(cfg.Server.HTTPSListen)
	}
	serverSummary := fmt.Sprintf(
		"隧道端口 / Tunnel     %s\nWeb HTTP              %s\nWeb HTTPS             %s\n在线客户端 / Online   %d",
		formatListenAddress(cfg.Server.TunnelListen),
		formatListenAddress(cfg.Server.HTTPListen),
		httpsListen,
		m.stats.OnlineConnections,
	)
	rows := make([][]string, 0, len(cfg.Connections))
	for _, c := range cfg.Connections {
		enabled := disabledTunnelStyle.Render("✗")
		if c.Enabled {
			enabled = enabledTunnelStyle.Render("✓")
		}
		status := "offline"
		if m.isConnectionOnline(c.ID) {
			status = "online"
		}
		connType := c.Type
		if connType == "" {
			connType = config.ConnectionTypeTCP
		}
		portStr := "-"
		if c.ListenPort > 0 {
			portStr = strconv.Itoa(c.ListenPort)
		}
		rows = append(rows, []string{
			connType,
			c.Name,
			c.Host,
			portStr,
			c.Target,
			enabled,
			status,
		})
	}
	tunnels := sectionBox("隧道 / Tunnels", renderTable(
		[]string{"Type", "Name", "Host", "Port", "Target", "Enabled", "Status"},
		rows,
		m.cursor[pageConnections],
		"暂无隧道，按 a 新建。/ No tunnels. Press 'a' to create one.",
	))
	return lipgloss.JoinVertical(lipgloss.Left, sectionBox("服务端 / Server", serverSummary), "", tunnels)
}

func (m *Model) renderStatusPage() string {
	statsLines := fmt.Sprintf(
		"Online Clients      %d\nTotal Tunnels       %d\nActive Tunnels      %d\nActive Streams      %d\nBytes Sent          %s\nBytes Received      %s",
		m.stats.OnlineConnections, m.stats.TotalConnections, m.stats.ActiveTunnels,
		m.stats.ActiveStreams, formatBytes(m.stats.BytesSent), formatBytes(m.stats.BytesReceived),
	)
	statsBox := sectionBox("统计 / Stats", statsLines)

	rows := make([][]string, 0, len(m.onlineClients))
	for _, c := range m.onlineClients {
		rows = append(rows, []string{
			c.Name, c.ConnectionID, c.RemoteAddr,
			strconv.Itoa(c.TunnelCount),
			strconv.FormatInt(int64(c.ActiveStreams), 10),
			formatBytes(c.BytesSent),
			formatBytes(c.BytesReceived),
			c.ConnectedAt.Format("15:04:05"),
			formatDuration(time.Since(c.ConnectedAt)),
		})
	}
	onlineTable := sectionBox("在线连接 / Online Connections", renderTable(
		[]string{"Name", "Connection ID", "Remote Addr", "Tunnels", "Active Streams", "Bytes Sent", "Bytes Received", "Connected Since", "Duration"},
		rows,
		m.cursor[pageStatus],
		"暂无在线连接。/ No online connections.",
	))

	if !m.showDetail {
		return lipgloss.JoinVertical(lipgloss.Left, statsBox, "", onlineTable)
	}

	i := m.cursor[pageStatus]
	if i < 0 || i >= len(m.onlineClients) {
		return lipgloss.JoinVertical(lipgloss.Left, statsBox, "", onlineTable)
	}
	c := m.onlineClients[i]

	detailRows := make([][]string, 0, len(c.Tunnels))
	for _, t := range c.Tunnels {
		detailRows = append(detailRows, []string{
			t.RemoteAddr,
			formatDuration(time.Since(t.ConnectedAt)),
			strconv.FormatInt(int64(t.ActiveStreams), 10),
			strconv.FormatInt(int64(t.TotalStreams), 10),
			formatBytes(t.BytesSent),
			formatBytes(t.BytesReceived),
			t.LastPongAt.Format("15:04:05"),
		})
	}
	detailTitle := fmt.Sprintf("隧道详情 / Tunnels for %s (%s)", c.Name, c.ConnectionID)
	detail := sectionBox(detailTitle, renderTable(
		[]string{"Remote Addr", "Connected Duration", "Active Streams", "Total Streams", "Bytes Sent", "Bytes Received", "Last Pong"},
		detailRows,
		-1,
		"暂无隧道。/ No tunnels.",
	))

	return lipgloss.JoinVertical(lipgloss.Left, statsBox, "", onlineTable, "", detail)
}

func (m *Model) renderLogs() string {
	requestRows := make([]string, 0, serverLogRowCount-1)
	for _, line := range m.logLines {
		entry, ok := parseRequestLogLine(line)
		if !ok {
			continue
		}
		requestRows = append(requestRows, renderRequestLogEntry(entry))
	}
	if len(requestRows) > serverLogRowCount-1 {
		requestRows = requestRows[len(requestRows)-(serverLogRowCount-1):]
	}
	lines := []string{headerStyle.Render(renderRequestLogRow("Time", "Method", "Address", "Response Time"))}
	if len(requestRows) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(mutedColor).Render("暂无请求日志 / no request logs yet"))
	}
	lines = append(lines, requestRows...)
	for len(lines) < serverLogRowCount {
		lines = append(lines, "")
	}
	return sectionBox("日志 / Logs", strings.Join(lines, "\n"))
}

func (m *Model) renderSettings() string {
	cfg := m.store.Get()

	serverLines := fmt.Sprintf(
		"Tunnel Listen       %s\nWeb HTTP Listen     %s\nWeb HTTPS Listen    %s",
		formatListenAddress(cfg.Server.TunnelListen),
		formatListenAddress(cfg.Server.HTTPListen),
		formatListenAddress(cfg.Server.HTTPSListen),
	)
	serverBox := sectionBox("服务器 / Server", serverLines)

	note := lipgloss.NewStyle().Italic(true).Faint(true).Render(
		"提示：Tunnel Listen 保存后立即热切换；Web 监听地址将在 HTTP/HTTPS 转发服务接入后生效。",
	)

	return lipgloss.JoinVertical(lipgloss.Left, serverBox, "", note)
}

func (m *Model) renderInputModal() string {
	var b strings.Builder
	b.WriteString(m.inputTitle)
	b.WriteString("\n\n")
	for i, in := range m.inputs {
		marker := "  "
		if i == m.inputIdx {
			marker = "▸ "
		}
		inputView := in.View()
		if i == 0 && (m.inputKind == inputNewConn || m.inputKind == inputEditConn) {
			inputView = renderProtocolSelector(m.protocolIdx)
		}
		fmt.Fprintf(&b, "%s%-26s %s\n", marker, m.inputLabels[i]+":", inputView)
	}
	if m.inputKind == inputNewConn || m.inputKind == inputEditConn {
		b.WriteString("\n协议项使用 ←/→ 选择  ↑/↓ 或 Tab:切换输入项  Enter:提交  Esc:取消")
	} else {
		b.WriteString("\nTab:下一项  Enter:提交  Esc:取消")
	}
	return inputBoxStyle.Render(b.String())
}

func (m *Model) renderConfirmModal() string {
	return confirmBoxStyle.Render(fmt.Sprintf(
		"%s\n\n  y: 确认    n/Esc: 取消",
		m.confirmMsg,
	))
}

func (m *Model) renderTokenModal() string {
	title := "✦ Token / Connection Token"
	if m.token.rotated {
		title = "✦ Token 已重置 / Token Reset"
	}
	content := fmt.Sprintf(
		"%s\n\nConnection ID:  %s\nToken:          %s\n\n可按 t 随时查看此 Token。/ Press t to view this Token again.\n\n按任意键关闭 / Press any key to dismiss.",
		title, m.token.connID, m.token.token,
	)
	return tokenBoxStyle.Render(content)
}

// --- table helpers ------------------------------------------------------

// renderTable builds a simple aligned table with the selected row highlighted.
// emptyMsg is shown when there are no rows.
func renderTable(headers []string, rows [][]string, cursor int, emptyMsg string) string {
	if len(rows) == 0 {
		return lipgloss.NewStyle().Foreground(mutedColor).Render(emptyMsg)
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = lipgloss.Width(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && lipgloss.Width(c) > widths[i] {
				widths[i] = lipgloss.Width(c)
			}
		}
	}

	headerLine := joinRow(headers, widths)
	lines := []string{headerStyle.Render(headerLine)}
	for i, r := range rows {
		line := joinRow(r, widths)
		if i == cursor {
			line = selectedRowStyle.Render(line)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func joinRow(cols []string, widths []int) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		w := widths[i]
		if lipgloss.Width(c) > w {
			c = truncate(c, w)
		}
		parts[i] = lipgloss.NewStyle().Width(w).Render(c)
	}
	return strings.Join(parts, "  ")
}

func truncate(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	// Drop runes until it fits, leaving room for an ellipsis.
	out := []rune(s)
	for len(out) > 0 && lipgloss.Width(string(out))+1 > w {
		out = out[:len(out)-1]
	}
	if w > 0 {
		return string(out) + "…"
	}
	return ""
}

func sectionBox(title, body string) string {
	return titledBoxStyle.Render(
		lipgloss.JoinVertical(lipgloss.Left,
			boxTitleStyle.Render(title),
			body,
		),
	)
}

func newTextInput(placeholder string) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.Prompt = ""
	ti.CharLimit = 0
	ti.Width = 30
	return ti
}

// formatListenAddress 将空监听地址显示为未配置，避免首页出现空白端口。
func formatListenAddress(address string) string {
	if strings.TrimSpace(address) == "" {
		return "-"
	}
	return address
}

// validateListenAddress 校验监听地址必须包含端口，例如 :7001 或 0.0.0.0:7001。
func validateListenAddress(address string) error {
	if address == "" {
		return fmt.Errorf("listen address cannot be empty")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return fmt.Errorf("invalid listen address: %s", address)
	}
	return nil
}

// protocolIndex 返回协议在选择器中的位置，未知值回退到 TCP。
func protocolIndex(connType string) int {
	for i, candidate := range connectionTypes {
		if candidate == connType {
			return i
		}
	}
	for i, candidate := range connectionTypes {
		if candidate == config.ConnectionTypeTCP {
			return i
		}
	}
	return 0
}

// isConnectionType 判断协议是否属于界面允许提交的固定集合。
func isConnectionType(connType string) bool {
	for _, candidate := range connectionTypes {
		if candidate == connType {
			return true
		}
	}
	return false
}

// renderProtocolSelector 渲染单选协议控件，当前协议使用方括号标识。
func renderProtocolSelector(selected int) string {
	options := make([]string, 0, len(connectionTypes))
	for i, connType := range connectionTypes {
		label := strings.ToUpper(connType)
		if i == selected {
			options = append(options, selectedProtocolStyle.Render(label))
			continue
		}
		options = append(options, unselectedProtocolStyle.Render(label))
	}
	return lipgloss.JoinHorizontal(lipgloss.Left, options...)
}

// refreshLogLines 从后台日志文件加载最近的 100 条记录。
func (m *Model) refreshLogLines() {
	if m.logPath == "" {
		return
	}
	data, err := os.ReadFile(m.logPath)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 100 {
		lines = lines[len(lines)-100:]
	}
	m.logLines = lines
}

// requestLogEntry 是服务端 HTTP 转发完成后的日志展示数据。
type requestLogEntry struct {
	Time     time.Time
	Method   string
	Address  string
	Status   int
	Duration string
}

// parseRequestLogLine 从结构化 slog 输出中解析 HTTP 请求完成记录。
func parseRequestLogLine(line string) (requestLogEntry, bool) {
	if !strings.Contains(line, `msg="http request completed"`) {
		return requestLogEntry{}, false
	}
	timestamp, err := time.Parse(time.RFC3339Nano, slogField(line, "time"))
	if err != nil {
		return requestLogEntry{}, false
	}
	status, err := strconv.Atoi(slogField(line, "status"))
	if err != nil {
		return requestLogEntry{}, false
	}
	return requestLogEntry{
		Time:     timestamp,
		Method:   slogField(line, "method"),
		Address:  slogField(line, "address"),
		Status:   status,
		Duration: slogField(line, "duration"),
	}, true
}

// slogField 读取 slog 文本处理器输出中的单个键值字段。
func slogField(line, key string) string {
	prefix := key + "="
	index := strings.Index(line, prefix)
	if index < 0 {
		return ""
	}
	value := line[index+len(prefix):]
	if strings.HasPrefix(value, `"`) {
		end := strings.Index(value[1:], `"`)
		if end >= 0 {
			return value[1 : end+1]
		}
	}
	if end := strings.IndexByte(value, ' '); end >= 0 {
		return value[:end]
	}
	return value
}

// renderRequestLogRow 使用稳定列宽保证服务端日志字段对齐。
func renderRequestLogRow(values ...string) string {
	widths := []int{10, 8, 42}
	parts := make([]string, 0, len(values))
	for index, value := range values {
		if index < len(widths) {
			parts = append(parts, logCell(value, widths[index]))
			continue
		}
		parts = append(parts, value)
	}
	return strings.Join(parts, " ")
}

// logCell 按终端显示宽度补齐日志单元格，防止颜色代码影响对齐。
func logCell(value string, width int) string {
	padding := width - lipgloss.Width(value)
	if padding <= 0 {
		return value
	}
	return value + strings.Repeat(" ", padding)
}

// renderRequestLogEntry 按状态码和请求字段为服务端请求日志着色。
func renderRequestLogEntry(entry requestLogEntry) string {
	durationStyle := logInfoStyle
	if entry.Status >= http.StatusInternalServerError {
		durationStyle = logErrorStyle
	} else if entry.Status >= http.StatusBadRequest || isSlowRequest(entry.Duration) {
		durationStyle = logWarnStyle
	}
	return renderRequestLogRow(
		logTimeStyle.Render(logCell(entry.Time.Format("15:04:05"), 10)),
		logMethodStyle.Render(logCell(entry.Method, 8)),
		logAddressStyle.Render(logCell(truncate(entry.Address, 42), 42)),
		durationStyle.Render(entry.Duration),
	)
}

// isSlowRequest 判断结构化日志中的响应耗时是否达到告警阈值。
func isSlowRequest(value string) bool {
	duration, err := time.ParseDuration(value)
	return err == nil && duration >= time.Second
}

// generateID returns a random hex id with the given prefix.
func generateID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return prefix + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return prefix + hex.EncodeToString(b)
}

// formatBytes formats a byte count into a human-readable string (e.g. 1.2KB, 3.4MB).
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// formatDuration formats a duration into a compact string (e.g. "2h15m", "3m45s").
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// --- styles -------------------------------------------------------------

var (
	primaryColor = lipgloss.Color("#7DD3FC")
	accentColor  = lipgloss.Color("#38BDF8")
	mutedColor   = lipgloss.Color("#64748B")
	panelColor   = lipgloss.Color("#1E293B")
	selectColor  = lipgloss.Color("#1D4ED8")
	warnColor    = lipgloss.Color("#FBBF24")
	errColor     = lipgloss.Color("#F87171")

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(primaryColor).
			Padding(0, 1)

	activeTabStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#0F172A")).
			Background(accentColor).
			Padding(0, 2)

	inactiveTabStyle = lipgloss.NewStyle().
				Foreground(mutedColor).
				Padding(0, 2)

	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E2E8F0")).
			Background(panelColor).
			MarginTop(1).
			Padding(0, 1)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#CBD5E1"))

	selectedRowStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#F8FAFC")).
				Background(selectColor)

	errorStyle = lipgloss.NewStyle().
			Foreground(errColor).
			MarginTop(1).
			Padding(0, 1)

	titledBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(mutedColor).
			Padding(0, 1).
			MarginTop(1)

	boxTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(accentColor).
			MarginBottom(1)

	inputBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(accentColor).
			Padding(1, 2).
			MarginTop(1)

	confirmBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(warnColor).
			Padding(1, 2).
			MarginTop(1)

	tokenBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(warnColor).
			Padding(1, 2).
			MarginTop(1)

	versionStyle = lipgloss.NewStyle().
			Foreground(mutedColor).
			MarginLeft(1)

	selectedProtocolStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#0F172A")).
				Background(accentColor).
				Padding(0, 2)

	unselectedProtocolStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#CBD5E1")).
				Background(panelColor).
				Padding(0, 2)

	logInfoStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#86EFAC"))
	logWarnStyle        = lipgloss.NewStyle().Foreground(warnColor)
	logErrorStyle       = lipgloss.NewStyle().Foreground(errColor).Bold(true)
	logDebugStyle       = lipgloss.NewStyle().Foreground(mutedColor)
	logTimeStyle        = lipgloss.NewStyle().Foreground(mutedColor)
	logMethodStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#86EFAC"))
	logAddressStyle     = lipgloss.NewStyle().Foreground(primaryColor)
	githubStyle         = lipgloss.NewStyle().Foreground(mutedColor).Padding(0, 1)
	enabledTunnelStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#86EFAC"))
	disabledTunnelStyle = lipgloss.NewStyle().Bold(true).Foreground(errColor)
)
