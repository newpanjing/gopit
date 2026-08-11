package tunnel

import "errors"

var (
	ErrTunnelClosed    = errors.New("tunnel closed")
	ErrAuthFailed      = errors.New("authentication failed")
	ErrAuthTimeout     = errors.New("authentication timeout")
	ErrProtocolVersion = errors.New("protocol version mismatch")
)
