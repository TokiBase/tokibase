package netguard

import (
	"net/netip"
	"testing"
)

func TestBlockedIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true, "169.254.169.254": true,
		"100.64.0.1": true, "0.0.0.0": true, "224.0.0.1": true, "240.0.0.1": true, "198.18.0.1": true,
		"::1": true, "fe80::1": true, "fc00::1": true, "fec0::1": true, "::ffff:127.0.0.1": true,
		"64:ff9b::7f00:1": true, "2002:7f00:1::": true, "2001::1": true,
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700:4700::1111": false,
	} {
		if got := BlockedIP(netip.MustParseAddr(ip)); got != want {
			t.Errorf("BlockedIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestControlFuncHonorsEnv(t *testing.T) {
	c := ControlFunc("TEST_ALLOW_PRIVATE", nil)
	if err := c("tcp", "127.0.0.1:80", nil); err == nil {
		t.Fatal("loopback must be blocked")
	}
	if err := c("tcp", "8.8.8.8:80", nil); err != nil {
		t.Fatalf("public address blocked: %v", err)
	}
	t.Setenv("TEST_ALLOW_PRIVATE", "1")
	if err := c("tcp", "127.0.0.1:80", nil); err != nil {
		t.Fatalf("env must lift the guard: %v", err)
	}
}
