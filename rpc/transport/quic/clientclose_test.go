package rpcquic_test

import (
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

func TestCloseDuringHandshake(t *testing.T) {
	f := newEarlyFixture(t, 0, true, nil)
	f.d.Enable0RTT = false
	ready, release := f.pc.HoldWrites()
	defer release()
	cc := f.client(t)
	ctx := transporttest.Context(t)
	result := make(chan error, 1)
	go func() { result <- earlyCall(ctx, cc, "Unary", "handshake") }()
	await(ctx, t, ready)
	if err := cc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := await(ctx, t, result); status.Code(err) != codes.Canceled {
		t.Fatalf("Close during TLS handshake: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("Close waited for the caller's deadline")
	}
	if f.calls.Load() != 0 {
		t.Fatal("handler ran before the ordinary handshake completed")
	}
}
