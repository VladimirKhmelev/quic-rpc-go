package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
)

func TestMessageLimitSeries(t *testing.T) {
	s := newTestSeries(t)
	changed := strings.ReplaceAll(testMatrix, "tcp: aligned}", "tcp: aligned, max_recv_msg_bytes: 9437184, max_send_msg_bytes: 9437184}")
	changed = strings.ReplaceAll(changed, "tcp: default}", "tcp: default, max_recv_msg_bytes: 9437184, max_send_msg_bytes: 9437184}")
	s.setMatrix(strings.ReplaceAll(changed, "loads: [small]", "loads: [heavy-8m]"))
	_, lines, err := s.run(seriesConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "run ") && !strings.Contains(line, "-max-recv-msg-bytes 9437184 -max-send-msg-bytes 9437184") {
			t.Fatal(line)
		}
	}
	path := filepath.Join(s.dir, "series.json")
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved seriesFile
	if err := json.Unmarshal(bytes, &saved); err != nil {
		t.Fatal(err)
	}
	for _, r := range saved.Runs {
		if r.MessageLimits != (common.MessageLimits{MaxRecvMsgBytes: 9 << 20, MaxSendMsgBytes: 9 << 20}) {
			t.Fatalf("lost effective limits: %+v", r)
		}
	}
	// A different limit changes the experiment, even when its load still fits.
	s.setMatrix(strings.ReplaceAll(strings.ReplaceAll(changed, "loads: [small]", "loads: [heavy-8m]"), "9437184", "10485760"))
	if _, lines, err := s.run(seriesConfig{}); err == nil || !strings.Contains(err.Error(), "another matrix") || len(lines) != 0 {
		t.Fatalf("resumed changed limits: %v, %v", lines, err)
	}
	// Tampering with just a planned run is detected too.
	saved.Runs[0].MaxSendMsgBytes = 0
	bytes, err = json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	s.setMatrix(strings.ReplaceAll(changed, "loads: [small]", "loads: [heavy-8m]"))
	if _, _, err := s.run(seriesConfig{}); err == nil || !strings.Contains(err.Error(), "differs from its variant") {
		t.Fatalf("accepted changed planned limits: %v", err)
	}
}

func TestMessageLimitCLIValidation(t *testing.T) {
	for _, flag := range []string{"-max-recv-msg-bytes", "-max-send-msg-bytes"} {
		for _, value := range []string{"-1", "64"} {
			code, err := run([]string{"run", "-transport", "quic", "-load", "small", flag, value})
			if code != 2 || err == nil {
				t.Fatalf("%s %s: %d, %v", flag, value, code, err)
			}
		}
	}
	code, err := run([]string{"run", "-transport", "quic", "-load", "heavy-8m"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "heavy size") {
		t.Fatalf("8 MiB without raised limits: %d, %v", code, err)
	}
}
