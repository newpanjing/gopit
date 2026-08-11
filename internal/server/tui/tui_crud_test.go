package tui

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"gopit/internal/config"
	"gopit/internal/server/app"
	"gopit/internal/server/configstore"
	"gopit/internal/tunnel"
)

// TestTUITunnelTerminology 确保服务端规则列表统一使用“隧道”术语，且附加式界面不展示内部同步提示。
func TestTUITunnelTerminology(t *testing.T) {
	_, store, _ := newTestApp(t)
	m := NewAttached(store, nil, func() (app.Stats, []app.OnlineClientInfo, error) {
		return app.Stats{}, nil, nil
	})

	view := m.View()
	if !strings.Contains(view, "隧道 / Tunnels") {
		t.Fatalf("view missing tunnel title: %q", view)
	}
	if strings.Contains(view, "已附加后台服务") {
		t.Fatalf("view contains internal attachment hint: %q", view)
	}
}

func newTestApp(t *testing.T) (*app.App, *configstore.Store, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "server.yaml")

	cfg := &config.ServerConfig{
		Server: config.ServerSection{
			Host:          "0.0.0.0",
			Port:          7001,
			TunnelListen:  ":0",
			ConfigVersion: 1,
		},
		Connections: []config.Connection{},
	}
	if err := config.SaveServerConfig(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store, err := configstore.NewStore(cfgPath, logger)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	a := app.New(store, logger, tunnel.DefaultConfig())
	if err := a.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { a.Stop() })
	return a, store, cfgPath
}

// sendKey sends a key and optionally pumps the resulting command.
// Cursor blink commands block forever, so we use a short timeout to skip them.
// For async operations (like CreateConnection), use pump=true with a longer timeout.
func sendKey(t *testing.T, m *Model, key string, pump bool) {
	t.Helper()
	var keyMsg tea.KeyMsg
	switch key {
	case "enter":
		keyMsg.Type = tea.KeyEnter
	case "esc":
		keyMsg.Type = tea.KeyEsc
	case "tab":
		keyMsg.Type = tea.KeyTab
	case " ":
		keyMsg.Type = tea.KeySpace
	case "left":
		keyMsg.Type = tea.KeyLeft
	case "right":
		keyMsg.Type = tea.KeyRight
	default:
		keyMsg.Type = tea.KeyRunes
		keyMsg.Runes = []rune(key)
	}
	model, cmd := m.Update(keyMsg)
	*m = *(model.(*Model))

	if !pump || cmd == nil {
		return
	}

	// Execute the command with a generous timeout (Argon2id hashing takes time).
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		if msg != nil {
			model, nextCmd := m.Update(msg)
			*m = *(model.(*Model))
			// Pump one more level for any follow-up commands.
			if nextCmd != nil {
				ch2 := make(chan tea.Msg, 1)
				go func() { ch2 <- nextCmd() }()
				select {
				case msg2 := <-ch2:
					if msg2 != nil {
						model2, _ := m.Update(msg2)
						*m = *(model2.(*Model))
					}
				case <-time.After(2 * time.Second):
				}
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("command timed out")
	}
}

func sendKeyNoPump(t *testing.T, m *Model, key string) {
	sendKey(t, m, key, false)
}

func sendText(t *testing.T, m *Model, text string) {
	t.Helper()
	for _, r := range text {
		keyMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
		model, _ := m.Update(keyMsg)
		*m = *(model.(*Model))
	}
}

func sendBackspace(t *testing.T, m *Model) {
	t.Helper()
	keyMsg := tea.KeyMsg{Type: tea.KeyBackspace}
	model, _ := m.Update(keyMsg)
	*m = *(model.(*Model))
}

func TestTUI_CreateConnection(t *testing.T) {
	a, store, _ := newTestApp(t)

	m := New(a, store, nil)
	m.Init()

	if m.page != pageConnections {
		t.Fatalf("expected pageConnections, got %d", m.page)
	}
	if len(store.Get().Connections) != 0 {
		t.Fatalf("expected 0 connections")
	}

	// 按 'a' 进入新建表单。
	sendKeyNoPump(t, m, "a")
	if m.mode != modeInput {
		t.Fatalf("after 'a', expected modeInput, got %d", m.mode)
	}
	if m.inputKind != inputNewConn {
		t.Fatalf("expected inputNewConn, got %d", m.inputKind)
	}

	// 协议默认为 TCP，Tab 后输入名称。
	if m.inputs[0].Value() != config.ConnectionTypeTCP {
		t.Fatalf("protocol = %q, want %q", m.inputs[0].Value(), config.ConnectionTypeTCP)
	}
	sendKeyNoPump(t, m, "tab")
	sendText(t, m, "my-tunnel")
	sendKeyNoPump(t, m, "tab")
	sendKeyNoPump(t, m, "tab")
	sendText(t, m, "39001")
	sendKeyNoPump(t, m, "tab")
	sendText(t, m, "localhost:8080")
	sendKeyNoPump(t, m, "tab") // 到 Token (留空自动生成)

	if m.inputs[1].Value() != "my-tunnel" {
		t.Fatalf("name = %q, want %q", m.inputs[1].Value(), "my-tunnel")
	}
	if m.inputs[3].Value() != "39001" {
		t.Fatalf("port = %q, want %q", m.inputs[3].Value(), "39001")
	}
	if m.inputs[4].Value() != "localhost:8080" {
		t.Fatalf("target = %q, want %q", m.inputs[4].Value(), "localhost:8080")
	}

	// Enter 提交 (需要 pump 执行 createConnection)
	sendKey(t, m, "enter", true)

	conns := store.Get().Connections
	if len(conns) != 1 {
		t.Fatalf("expected 1 connection, got %d (errMsg=%q, mode=%d)", len(conns), m.errMsg, m.mode)
	}
	if conns[0].Name != "my-tunnel" {
		t.Fatalf("name = %q, want %q", conns[0].Name, "my-tunnel")
	}
	if conns[0].ListenPort != 39001 {
		t.Fatalf("port = %d, want %d", conns[0].ListenPort, 39001)
	}
	if conns[0].Token == "" {
		t.Fatal("expected stored token")
	}

	// 关闭创建结果后，可通过 t 重复查看同一 Token。
	sendKeyNoPump(t, m, "x")
	sendKeyNoPump(t, m, "t")
	if m.mode != modeToken {
		t.Fatalf("after 't', expected modeToken, got %d", m.mode)
	}
	if m.token.token != conns[0].Token {
		t.Fatalf("displayed token = %q, want %q", m.token.token, conns[0].Token)
	}
}

func TestTUI_NewConnectionShortcutAndProtocolSelector(t *testing.T) {
	a, store, _ := newTestApp(t)
	m := New(a, store, nil)

	// n 不再用于新增连接。
	sendKeyNoPump(t, m, "n")
	if m.mode != modeNormal {
		t.Fatalf("after 'n', expected modeNormal, got %d", m.mode)
	}

	sendKeyNoPump(t, m, "a")
	if m.mode != modeInput {
		t.Fatalf("after 'a', expected modeInput, got %d", m.mode)
	}
	if got := m.inputs[0].Value(); got != config.ConnectionTypeTCP {
		t.Fatalf("default protocol = %q, want %q", got, config.ConnectionTypeTCP)
	}

	// 选择器顺序为 HTTP、TCP、UDP，TCP 向右切换到 UDP。
	sendKeyNoPump(t, m, "right")
	if got := m.inputs[0].Value(); got != config.ConnectionTypeUDP {
		t.Fatalf("selected protocol = %q, want %q", got, config.ConnectionTypeUDP)
	}
	sendText(t, m, "invalid")
	if got := m.inputs[0].Value(); got != config.ConnectionTypeUDP {
		t.Fatalf("protocol selector accepted text input: %q", got)
	}
}

func TestTUI_ArrowNavigation(t *testing.T) {
	a, store, _ := newTestApp(t)
	m := New(a, store, nil)

	sendKeyNoPump(t, m, "right")
	if m.page != pageStatus {
		t.Fatalf("after right, page = %d, want %d", m.page, pageStatus)
	}
	sendKeyNoPump(t, m, "left")
	if m.page != pageConnections {
		t.Fatalf("after left, page = %d, want %d", m.page, pageConnections)
	}

	sendKeyNoPump(t, m, "a")
	sendKeyNoPump(t, m, "down")
	if m.inputIdx != 1 {
		t.Fatalf("after down, input index = %d, want 1", m.inputIdx)
	}
	sendKeyNoPump(t, m, "up")
	if m.inputIdx != 0 {
		t.Fatalf("after up, input index = %d, want 0", m.inputIdx)
	}
}

func TestTUI_EditConnection(t *testing.T) {
	a, store, _ := newTestApp(t)

	_, _, err := a.CreateConnection("orig", config.ConnectionTypeTCP, "", 39002, "localhost:3000", false, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	m := New(a, store, nil)
	m.Init()

	conns := store.Get().Connections
	if len(conns) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(conns))
	}

	// 按 'e' 编辑
	sendKeyNoPump(t, m, "e")
	if m.mode != modeInput {
		t.Fatalf("after 'e', expected modeInput, got %d", m.mode)
	}
	if m.inputKind != inputEditConn {
		t.Fatalf("expected inputEditConn, got %d", m.inputKind)
	}

	// 验证预填值
	if m.inputs[0].Value() != config.ConnectionTypeTCP {
		t.Fatalf("pre-filled protocol = %q, want %q", m.inputs[0].Value(), config.ConnectionTypeTCP)
	}
	if m.inputs[1].Value() != "orig" {
		t.Fatalf("pre-filled name = %q, want %q", m.inputs[1].Value(), "orig")
	}
	if m.inputs[3].Value() != "39002" {
		t.Fatalf("pre-filled port = %q, want %q", m.inputs[3].Value(), "39002")
	}
	if m.inputs[4].Value() != "localhost:3000" {
		t.Fatalf("pre-filled target = %q, want %q", m.inputs[4].Value(), "localhost:3000")
	}

	// Tab 到 target 字段
	sendKeyNoPump(t, m, "tab")
	sendKeyNoPump(t, m, "tab")
	sendKeyNoPump(t, m, "tab")
	sendKeyNoPump(t, m, "tab")

	// 用退格清空 target
	for m.inputs[4].Value() != "" {
		sendBackspace(t, m)
	}
	sendText(t, m, "localhost:9999")

	if m.inputs[4].Value() != "localhost:9999" {
		t.Fatalf("target = %q, want %q", m.inputs[4].Value(), "localhost:9999")
	}

	// Enter 提交
	sendKey(t, m, "enter", true)

	conns = store.Get().Connections
	if len(conns) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(conns))
	}
	if conns[0].Target != "localhost:9999" {
		t.Fatalf("target = %q, want %q", conns[0].Target, "localhost:9999")
	}
}

func TestTUI_DeleteConnection(t *testing.T) {
	a, store, _ := newTestApp(t)

	_, _, err := a.CreateConnection("to-delete", config.ConnectionTypeTCP, "", 39003, "localhost:4000", false, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	m := New(a, store, nil)
	m.Init()

	if len(store.Get().Connections) != 1 {
		t.Fatalf("expected 1 connection")
	}

	// 按 'd' 删除
	sendKeyNoPump(t, m, "d")
	if m.mode != modeConfirm {
		t.Fatalf("after 'd', expected modeConfirm, got %d", m.mode)
	}

	// 按 'y' 确认 (需要 pump 执行 deleteConnection)
	sendKey(t, m, "y", true)

	conns := store.Get().Connections
	if len(conns) != 0 {
		t.Fatalf("after delete, expected 0 connections, got %d", len(conns))
	}
}
