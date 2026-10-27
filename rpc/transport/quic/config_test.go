package rpcquic

import (
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func TestKeepAlivePeriod(t *testing.T) {
	for _, tt := range []struct {
		name   string
		conf   *quic.Config
		client bool
		want   time.Duration
	}{
		{"client, no config", nil, true, 15 * time.Second},
		{"client, idle timeout set", &quic.Config{MaxIdleTimeout: 10 * time.Second}, true, 5 * time.Second},
		{"client, period set", &quic.Config{KeepAlivePeriod: 3 * time.Second}, true, 3 * time.Second},
		{"client, turned off", &quic.Config{KeepAlivePeriod: -1}, true, 0},
		{"server", nil, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := configureQUIC(tt.conf, tt.client).KeepAlivePeriod; got != tt.want {
				t.Errorf("KeepAlivePeriod = %v, want %v", got, tt.want)
			}
		})
	}
}
