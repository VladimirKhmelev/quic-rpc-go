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
		want := common.ClientOptions{PoolSize: 1, PoolPolicy: "single"}
		if tr == common.GRPCH2Pool {
			want = common.ClientOptions{PoolSize: 4, PoolPolicy: "round-robin"}
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
