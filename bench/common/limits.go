package common

import (
	"errors"
	"fmt"
	"math"
)

// MessageLimits records serialized protobuf message limits, shared by the
// client and server of a benchmark. Zero receive selects 4 MiB; zero send
// preserves unlimited sending. These are independent of QUIC flow control.
type MessageLimits struct {
	MaxRecvMsgBytes int `json:"max_recv_msg_bytes" yaml:"max_recv_msg_bytes"`
	MaxSendMsgBytes int `json:"max_send_msg_bytes" yaml:"max_send_msg_bytes"`
}

// Resolve validates limits and fills the receive default.
func (l MessageLimits) Resolve() (MessageLimits, error) {
	if l.MaxRecvMsgBytes == 0 {
		l.MaxRecvMsgBytes = 4 << 20
	}
	if l.MaxRecvMsgBytes <= 64 || l.MaxRecvMsgBytes > math.MaxInt32 || l.MaxSendMsgBytes < 0 || l.MaxSendMsgBytes > math.MaxInt32 || l.MaxSendMsgBytes > 0 && l.MaxSendMsgBytes <= 64 {
		return MessageLimits{}, fmt.Errorf("message limits: receive must be 65..%d bytes; send must be 0 (unlimited) or 65..%d bytes", math.MaxInt32, math.MaxInt32)
	}
	return l, nil
}

func resolveMessageLimits(limits []MessageLimits) (MessageLimits, error) {
	if len(limits) > 1 {
		return MessageLimits{}, errors.New("common: want at most one MessageLimits")
	}
	var l MessageLimits
	if len(limits) == 1 {
		l = limits[0]
	}
	return l.Resolve()
}

// MaxPayload leaves 64 bytes for protobuf tags, lengths, response_size and
// sequence numbers. It is deliberately conservative and preserves the legacy
// MaxResponse bound. Resolve must be called first.
func (l MessageLimits) MaxPayload() int {
	n := l.MaxRecvMsgBytes
	if l.MaxSendMsgBytes > 0 && l.MaxSendMsgBytes < n {
		n = l.MaxSendMsgBytes
	}
	return n - 64
}
