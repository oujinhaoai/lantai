package application

import "testing"

func TestOperationsListenerCannotBindOutsideLoopback(t *testing.T) {
	for _, address := range []string{":9090", "0.0.0.0:9090", "[::]:9090", "192.0.2.1:9090", "localhost:9090", "example.test:9090", "127.0.0.1:-1", "127.0.0.1:65536"} {
		if err := validateAddress(address, true); err == nil {
			t.Errorf("unsafe operations address accepted: %s", address)
		}
	}
	for _, address := range []string{"127.0.0.1:9090", "[::1]:9090", "127.0.0.1:0"} {
		if err := validateAddress(address, true); err != nil {
			t.Errorf("valid loopback rejected: %s: %v", address, err)
		}
	}
}
