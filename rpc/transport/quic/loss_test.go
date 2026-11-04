package rpcquic_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

func TestRPCPacketLoss(t *testing.T) {
	f := newEarlyFixture(t, 0, true, nil)
	f.d.Enable0RTT = false
	f.pc.SetDropEvery(20) // Five percent of server datagrams, retransmissions included.
	cc := f.client(t)
	ctx := transporttest.Context(t)
	for _, kind := range []string{"Unary", "ServerStream", "ClientStream", "Bidi"} {
		for i := range 4 {
			value := fmt.Sprintf("%s-%d:", kind, i) + strings.Repeat("x", 64<<10)
			if err := earlyCall(ctx, cc, kind, value, rpc.Idempotent(false)); err != nil {
				t.Fatalf("%s under loss: %v", kind, err)
			}
		}
	}
	if f.pc.Dropped() == 0 {
		t.Fatal("the test did not drop any packets")
	}
	t.Run("deadline", func(t *testing.T) {
		_, release := f.pc.HoldWrites()
		defer release()
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if err := earlyCall(short, cc, "Unary", "deadline"); status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("deadline while packets are held: %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		ready, release := f.pc.HoldWrites()
		defer release()
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- earlyCall(callCtx, cc, "Unary", "cancel") }()
		await(ctx, t, ready)
		cancel()
		if err := await(ctx, t, result); status.Code(err) != codes.Canceled {
			t.Fatalf("cancel while packets are held: %v", err)
		}
	})
	if err := earlyCall(ctx, cc, "Unary", "after cancellation"); err != nil {
		t.Fatal("connection did not recover:", err)
	}
}
