package discovery

import "testing"

func TestPortFromListenAddress(t *testing.T) {
	cases := map[string]int{
		":8787":            8787,
		"0.0.0.0:8787":     8787,
		"127.0.0.1:9000":   9000,
		"[::]:8787":        8787,
		"192.168.1.10:443": 443,
	}
	for addr, want := range cases {
		got, err := PortFrom(addr)
		if err != nil {
			t.Errorf("PortFrom(%q): %v", addr, err)
			continue
		}
		if got != want {
			t.Errorf("PortFrom(%q) = %d, want %d", addr, got, want)
		}
	}
}

func TestPortFromRejectsUnusableAddresses(t *testing.T) {
	for _, addr := range []string{"", "8787", ":", ":not-a-port", ":0", ":70000"} {
		if _, err := PortFrom(addr); err == nil {
			t.Errorf("PortFrom(%q) returned no error", addr)
		}
	}
}

func TestDefaultInstanceIsNonEmptyAndPrefixed(t *testing.T) {
	got := DefaultInstance()
	if got == "" {
		t.Fatal("DefaultInstance is empty")
	}
	if len(got) < len("photod-") || got[:len("photod-")] != "photod-" {
		if got != "photod" {
			t.Errorf("DefaultInstance = %q, want a photod-prefixed name", got)
		}
	}
}
