package protocol

// 控制帧类型
const (
	MsgTypeAuth          = "auth"
	MsgTypeAuthResult    = "auth_result"
	MsgTypeRulesSnapshot = "rules_snapshot"
	MsgTypeRulesChanged  = "rules_changed"
	MsgTypeRulesSyncReq  = "rules_sync_request"
	MsgTypePing          = "ping"
	MsgTypePong          = "pong"
	MsgTypeError         = "error"
)

// ControlMessage 是所有控制帧的通用 JSON 结构。
// 不同 type 使用不同字段，未使用的字段为零值。
type ControlMessage struct {
	Type string `json:"type"`

	// auth
	Version    int         `json:"version,omitempty"`
	ClientID   string      `json:"client_id,omitempty"`
	ClientInfo *ClientInfo `json:"client_info,omitempty"`
	Token      string      `json:"token,omitempty"`

	// auth_result
	Success bool   `json:"success,omitempty"`
	Error   string `json:"error,omitempty"`

	// rules_snapshot / rules_changed
	ConfigVersion int64        `json:"config_version,omitempty"`
	Routes        []RouteEntry `json:"routes,omitempty"`

	// ping / pong
	Timestamp int64 `json:"timestamp,omitempty"`

	// error
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ClientInfo 携带客户端元信息，在 auth 帧中发送。
type ClientInfo struct {
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

// RouteEntry 是规则快照中的单条规则。
type RouteEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Protocol    string `json:"protocol"` // "http" 或 "tcp"
	Host        string `json:"host,omitempty"`
	ForwardHost bool   `json:"forward_host,omitempty"`
	ListenPort  int    `json:"listen_port,omitempty"`
	Target      string `json:"target"`
}

// --- 构造辅助函数 ---

func NewAuth(version int, clientID, token string, info *ClientInfo) ControlMessage {
	return ControlMessage{
		Type:       MsgTypeAuth,
		Version:    version,
		ClientID:   clientID,
		Token:      token,
		ClientInfo: info,
	}
}

func NewAuthResult(success bool, clientID, errMsg string) ControlMessage {
	return ControlMessage{
		Type:     MsgTypeAuthResult,
		Success:  success,
		ClientID: clientID,
		Error:    errMsg,
	}
}

func NewRulesSnapshot(configVersion int64, routes []RouteEntry) ControlMessage {
	return ControlMessage{
		Type:          MsgTypeRulesSnapshot,
		ConfigVersion: configVersion,
		Routes:        routes,
	}
}

func NewRulesChanged(configVersion int64) ControlMessage {
	return ControlMessage{
		Type:          MsgTypeRulesChanged,
		ConfigVersion: configVersion,
	}
}

func NewRulesSyncRequest() ControlMessage {
	return ControlMessage{Type: MsgTypeRulesSyncReq}
}

func NewPing(timestamp int64) ControlMessage {
	return ControlMessage{Type: MsgTypePing, Timestamp: timestamp}
}

func NewPong(timestamp int64) ControlMessage {
	return ControlMessage{Type: MsgTypePong, Timestamp: timestamp}
}

func NewError(code, message string) ControlMessage {
	return ControlMessage{Type: MsgTypeError, Code: code, Message: message}
}
