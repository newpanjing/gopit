package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// 数据帧消息类型
const (
	DataOpenStream   uint8 = 0x01
	DataStreamData   uint8 = 0x02
	DataHalfClose    uint8 = 0x03
	DataStreamClose  uint8 = 0x04
	DataWindowUpdate uint8 = 0x05
)

// 标志位
const (
	FlagNone uint8 = 0x00
)

// DataFrameHeader 大小: msg_type(1) + stream_id(4) + flags(1) + data_len(4) = 10
const DataFrameHeaderSize = 10

// DataFrame 是一个解析后的数据帧。
type DataFrame struct {
	MsgType  uint8
	StreamID uint32
	Flags    uint8
	Data     []byte
}

// EncodeDataFrame 将 DataFrame 编码为 KindData 帧的 payload。
func EncodeDataFrame(df *DataFrame) []byte {
	buf := make([]byte, DataFrameHeaderSize+len(df.Data))
	buf[0] = df.MsgType
	binary.BigEndian.PutUint32(buf[1:5], df.StreamID)
	buf[5] = df.Flags
	binary.BigEndian.PutUint32(buf[6:10], uint32(len(df.Data)))
	copy(buf[10:], df.Data)
	return buf
}

// DecodeDataFrame 从 KindData 帧的 payload 解析 DataFrame。
func DecodeDataFrame(payload []byte) (*DataFrame, error) {
	if len(payload) < DataFrameHeaderSize {
		return nil, fmt.Errorf("data frame too short: %d", len(payload))
	}
	df := &DataFrame{
		MsgType:  payload[0],
		StreamID: binary.BigEndian.Uint32(payload[1:5]),
		Flags:    payload[5],
	}
	dataLen := binary.BigEndian.Uint32(payload[6:10])
	if int(dataLen) > len(payload)-DataFrameHeaderSize {
		return nil, fmt.Errorf("data frame length mismatch: declared=%d actual=%d", dataLen, len(payload)-DataFrameHeaderSize)
	}
	df.Data = payload[DataFrameHeaderSize : DataFrameHeaderSize+dataLen]
	return df, nil
}

// WriteDataFrame 将 DataFrame 编码并作为数据帧写入。
func WriteDataFrame(w io.Writer, df *DataFrame) error {
	payload := EncodeDataFrame(df)
	frame := &Frame{
		Version: ProtocolVersion,
		Kind:    KindData,
		Payload: payload,
	}
	return WriteFrame(w, frame)
}

// EncodeOpenStream 构造 open_stream 数据帧的 Data 字段。
func EncodeOpenStream(ruleID string) []byte {
	buf := make([]byte, 2+len(ruleID))
	binary.BigEndian.PutUint16(buf[0:2], uint16(len(ruleID)))
	copy(buf[2:], ruleID)
	return buf
}

// DecodeOpenStream 从 open_stream 帧的 Data 字段解析 rule_id。
func DecodeOpenStream(data []byte) (string, error) {
	if len(data) < 2 {
		return "", fmt.Errorf("open_stream data too short")
	}
	ruleIDLen := binary.BigEndian.Uint16(data[0:2])
	if int(ruleIDLen) > len(data)-2 {
		return "", fmt.Errorf("open_stream rule_id length mismatch")
	}
	return string(data[2 : 2+ruleIDLen]), nil
}

// EncodeWindowUpdate 构造 window_update 数据帧的 Data 字段。
func EncodeWindowUpdate(windowSize uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, windowSize)
	return buf
}

// DecodeWindowUpdate 从 window_update 帧的 Data 字段解析窗口大小。
func DecodeWindowUpdate(data []byte) (uint32, error) {
	if len(data) < 4 {
		return 0, fmt.Errorf("window_update data too short")
	}
	return binary.BigEndian.Uint32(data[0:4]), nil
}
