package transporttest

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestPacketConnDropsDatagrams(t *testing.T) {
	sender := NewPacketConn(t)
	sender.SetDropEvery(2)
	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	for _, value := range []string{"one", "two", "three"} {
		if n, err := sender.WriteTo([]byte(value), peer.LocalAddr()); err != nil || n != len(value) {
			t.Fatalf("WriteTo = %d, %v", n, err)
		}
	}
	buf := make([]byte, 32)
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	for _, want := range []string{"one", "three"} {
		n, _, err := peer.ReadFrom(buf)
		if err != nil || string(buf[:n]) != want {
			t.Fatalf("ReadFrom = %q, %v, want %q", buf[:n], err, want)
		}
	}
	_ = peer.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	if _, _, err := peer.ReadFrom(buf); err == nil {
		t.Fatal("dropped datagram reached the peer")
	} else if e, ok := errors.AsType[net.Error](err); !ok || !e.Timeout() {
		t.Fatal(err)
	}
	if sender.Dropped() != 1 {
		t.Fatalf("dropped %d, want 1", sender.Dropped())
	}
	_ = sender.Close()
	if _, err := sender.WriteTo([]byte("closed"), peer.LocalAddr()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("WriteTo after Close: %v", err)
	}
}
