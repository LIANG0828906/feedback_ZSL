package main

import "testing"

func TestValidateLoopbackAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "localhost:9000", "[::1]:8080"} {
		if err := validateLoopbackAddr(addr); err != nil {
			t.Fatalf("%s should be allowed: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8080", "192.168.1.10:8080", "bad-address"} {
		if err := validateLoopbackAddr(addr); err != nil {
			continue
		}
		t.Fatalf("%s should be rejected", addr)
	}
}
