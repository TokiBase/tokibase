//go:build !no_sync

package client

import (
	"strings"
	"testing"
)

func TestCleanErrorTruncatesAndStripsControlChars(t *testing.T) {
	got := cleanError("bad\x1b[31m\nthing\x00" + strings.Repeat("x", 2000))
	if len(got) > maxLastError || strings.ContainsAny(got, "\x1b\n\x00") {
		t.Fatalf("not cleaned: %q", got)
	}
	if !strings.HasPrefix(got, "bad [31m thing ") {
		t.Fatalf("prefix: %q", got)
	}
	if cleanError("é€") != "é€" {
		t.Fatal("multi-byte text must survive")
	}
}

func TestInsecureHostOK(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST": true, "127.0.0.1": true, "::1": true, "[::1]": true,
		"10.0.0.1": true, "172.16.5.4": true, "192.168.1.1": true, "fd00::1": true,
		"example.com": false, "8.8.8.8": false, "172.32.0.1": false, "100.64.0.1": false, "": false,
	} {
		if got := insecureHostOK(host); got != want {
			t.Errorf("insecureHostOK(%q) = %v, want %v", host, got, want)
		}
	}
}
