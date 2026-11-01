package httpgrpc

import "testing"

// TLS reads may split SETTINGS at any byte. Connect must not declare the
// connection ready until the entire initial frame has arrived.
func TestSettingsFragmented(t *testing.T) {
	frame := []byte{0, 0, 6, 4, 0, 0, 0, 0, 0, 0, 3, 0, 0, 0, 100}
	for split := 1; split < len(frame); split++ {
		c := newSettingsConn(nil)
		c.observe(frame[:split])
		select {
		case <-c.settings:
			t.Fatalf("ready after only %d of %d bytes", split, len(frame))
		default:
		}
		c.observe(frame[split:])
		select {
		case <-c.settings:
		default:
			t.Fatalf("not ready after complete frame split at %d", split)
		}
	}
}

func TestSettingsRejectACK(t *testing.T) {
	c := newSettingsConn(nil)
	c.observe([]byte{0, 0, 0, 4, 1, 0, 0, 0, 0})
	select {
	case <-c.settings:
		t.Fatal("ACK is not the peer's initial settings")
	default:
	}
}
