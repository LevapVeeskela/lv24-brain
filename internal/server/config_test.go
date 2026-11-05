package server

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfigClampsConnectorTimeout(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 5 * time.Minute},
		{"0s", 10 * time.Second},
		{"-1s", 10 * time.Second},
		{"1s", 10 * time.Second},
		{"10s", 10 * time.Second},
		{"17m", 17 * time.Minute},
		{"1h", time.Hour},
		{"2h", time.Hour},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := LoadConfig(nil, func(key string) string {
				switch key {
				case "BRAIN_JWT_SECRET":
					return strings.Repeat("x", 32)
				case "BRAIN_CONNECTOR_TIMEOUT":
					return tc.value
				default:
					return ""
				}
			})
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.ConnectorTimeout != tc.want {
				t.Fatalf("timeout = %s, want %s", cfg.ConnectorTimeout, tc.want)
			}
		})
	}
}

func TestLoadConfigRejectsMalformedConnectorTimeout(t *testing.T) {
	_, err := LoadConfig(nil, func(key string) string {
		if key == "BRAIN_CONNECTOR_TIMEOUT" {
			return "invalid"
		}
		if key == "BRAIN_JWT_SECRET" {
			return strings.Repeat("x", 32)
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "BRAIN_CONNECTOR_TIMEOUT") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigValidateRejectsUnboundedConnectorTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, 9 * time.Second, 2 * time.Hour} {
		cfg := DefaultConfig()
		cfg.ConnectorTimeout = timeout
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate accepted timeout %s", timeout)
		}
	}
}
