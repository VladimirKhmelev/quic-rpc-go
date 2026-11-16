package common_test

import (
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
)

func TestClientOptions(t *testing.T) {
	for _, tr := range common.Transports {
		o, err := (common.ClientOptions{}).Resolve(tr)
		if err != nil {
			t.Fatal(err)
		}
		want := common.ClientOptions{PoolSize: 1, PoolPolicy: "single", MessageLimits: common.MessageLimits{MaxRecvMsgBytes: 4 << 20}}
		if tr == common.GRPCH2Pool {
			want = common.ClientOptions{PoolSize: 4, PoolPolicy: "round-robin", MessageLimits: common.MessageLimits{MaxRecvMsgBytes: 4 << 20}}
		}
		if o != want {
			t.Fatalf("%s: %+v, want %+v", tr, o, want)
		}
	}
	for _, tc := range []struct {
		tr common.Transport
		o  common.ClientOptions
	}{
		{common.QUIC, common.ClientOptions{PoolSize: 2}},
		{common.GRPCH2, common.ClientOptions{PoolSize: 4}},
		{common.GRPCH2Pool, common.ClientOptions{PoolSize: -1}},
		{common.GRPCH2Pool, common.ClientOptions{PoolPolicy: "least-busy"}},
	} {
		if _, err := tc.o.Resolve(tc.tr); err == nil {
			t.Fatalf("accepted invalid setup: %s %+v", tc.tr, tc.o)
		}
	}
}

func TestMessageLimitOptions(t *testing.T) {
	for _, limits := range []common.MessageLimits{
		{MaxRecvMsgBytes: -1}, {MaxRecvMsgBytes: 64}, {MaxSendMsgBytes: -1}, {MaxSendMsgBytes: 64},
	} {
		for _, tr := range common.Transports {
			if _, err := (common.ClientOptions{MessageLimits: limits}).Resolve(tr); err == nil {
				t.Fatalf("accepted %s %+v", tr, limits)
			}
		}
	}
	l, err := (common.MessageLimits{MaxRecvMsgBytes: 9 << 20, MaxSendMsgBytes: 8 << 20}).Resolve()
	if err != nil || l.MaxPayload() != (8<<20)-64 {
		t.Fatalf("asymmetric limits: %+v, %v", l, err)
	}
}
