package main

import (
	"testing"
	"time"
)

func TestHeartbeatIntervalConfig(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  time.Duration
		valid bool
	}{{"", 30 * time.Second, true}, {"5s", 5 * time.Second, true}, {"5m", 5 * time.Minute, true}, {"4.99s", 0, false}, {"301s", 0, false}, {"30", 0, false}, {" 30s", 0, false}, {"-5s", 0, false}} {
		t.Run(tt.value, func(t *testing.T) {
			cfg, err := loadConfig(func(k string) string {
				if k == "ARKNET_HEARTBEAT_INTERVAL" {
					return tt.value
				}
				return ""
			})
			if (err == nil) != tt.valid {
				t.Fatalf("err=%v", err)
			}
			if tt.valid && cfg.heartbeatInterval != tt.want {
				t.Fatalf("interval=%v", cfg.heartbeatInterval)
			}
		})
	}
}
