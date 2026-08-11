package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	ProtocolVersion uint8 = 1

	KindControl uint8 = 0x01
	KindData    uint8 = 0x02

	// 默认帧大小限制
	MaxControlFrameSize = 1 << 20 // 1MB
	MaxDataPayloadSize  = 1 << 16 // 64KB
	HeaderSize          = 6       // version(1) + kind(1) + length(4)
)

var ErrFrameTooLarge = errors.New("frame too large")

// Frame 是线上的一个完整帧。
type Frame struct {
	Version uint8
	Kind    uint8
	Payload []byte
}

// ReadFrame 从 reader 读取一个完整帧。
func ReadFrame(r io.Reader, maxControlSize, maxDataSize int) (*Frame, error) {
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}

	version := header[0]
	kind := header[1]
	length := binary.BigEndian.Uint32(header[2:6])

	maxSize := maxControlSize
	if kind == KindData {
		maxSize = maxDataSize
	}
	if int(length) > maxSize {
		return nil, fmt.Errorf("%w: kind=%d length=%d max=%d", ErrFrameTooLarge, kind, length, maxSize)
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}

	return &Frame{Version: version, Kind: kind, Payload: payload}, nil
}

// WriteFrame 将一个帧写入 writer。
func WriteFrame(w io.Writer, f *Frame) error {
	header := make([]byte, HeaderSize)
	header[0] = f.Version
	header[1] = f.Kind
	binary.BigEndian.PutUint32(header[2:6], uint32(len(f.Payload)))

	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return err
		}
	}
	return nil
}

// MarshalControl 将控制消息序列化为 JSON 字节。
func MarshalControl(msg ControlMessage) ([]byte, error) {
	return json.Marshal(msg)
}

// DecodeControlPayload 将 JSON payload 解析为控制消息。
func DecodeControlPayload(payload []byte) (*ControlMessage, error) {
	var msg ControlMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, fmt.Errorf("decode control frame: %w", err)
	}
	return &msg, nil
}

// WriteControlFrame 将控制消息序列化为 JSON 并写入。
func WriteControlFrame(w io.Writer, msg ControlMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	frame := &Frame{
		Version: ProtocolVersion,
		Kind:    KindControl,
		Payload: data,
	}
	return WriteFrame(w, frame)
}

// ReadControlFrame 读取一个帧并解析为控制消息。
func ReadControlFrame(r io.Reader, maxControlSize, maxDataSize int) (*ControlMessage, error) {
	frame, err := ReadFrame(r, maxControlSize, maxDataSize)
	if err != nil {
		return nil, err
	}
	if frame.Kind != KindControl {
		return nil, fmt.Errorf("expected control frame, got kind=%d", frame.Kind)
	}
	var msg ControlMessage
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		return nil, fmt.Errorf("decode control frame: %w", err)
	}
	return &msg, nil
}
