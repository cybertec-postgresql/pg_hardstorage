package syslog_test

import (
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/syslog"
)

// airgapped: strict covers every outbound path; a syslog collector on a
// public host must be refused at build time, for every transport.
func TestSyslog_AirgapStrict_RefusesPublicCollector(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()

	build := func(proto, addr string) error {
		cfg := map[string]any{"protocol": proto, "address": addr}
		if proto == "tls" {
			cfg["tls"] = map[string]any{}
		}
		_, err := syslog.NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: cfg})
		return err
	}
	for _, proto := range []string{"udp", "tcp", "tls"} {
		for _, addr := range []string{"siem.example.com:6514", "8.8.8.8:514"} {
			if err := build(proto, addr); !errors.Is(err, airgap.ErrEndpointNotAllowed) {
				t.Errorf("%s %s under strict airgap: err = %v, want ErrEndpointNotAllowed", proto, addr, err)
			}
		}
		for _, addr := range []string{"127.0.0.1:514", "10.0.0.5:6514", "[::1]:514"} {
			if err := build(proto, addr); err != nil {
				t.Errorf("%s %s under strict airgap: %v", proto, addr, err)
			}
		}
	}
}
