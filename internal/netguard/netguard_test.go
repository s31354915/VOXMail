package netguard

import (
	"context"
	"net"
	"testing"
)

func TestDialRejectsLoopbackByDefault(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if conn, err := DialContext(context.Background(), "127.0.0.1", port, "", false); err == nil {
		conn.Close()
		t.Fatal("loopback dial was allowed")
	}
}

func TestDialAllowsExplicitPrivateEndpointForTests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			close(accepted)
			_ = conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	conn, err := DialContext(context.Background(), "127.0.0.1", port, "", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	<-accepted
}

func TestValidateAddressRejectsReservedAndMappedPrivateRanges(t *testing.T) {
	tests := []struct {
		name    string
		address string
		allowed bool
	}{
		{name: "loopback-v4", address: "127.0.0.1:993"},
		{name: "loopback-v6", address: "[::1]:993"},
		{name: "private-v4", address: "10.0.0.8:993"},
		{name: "metadata-link-local", address: "169.254.169.254:80"},
		{name: "cgnat", address: "100.64.0.8:993"},
		{name: "mapped-loopback", address: "[::ffff:127.0.0.1]:993"},
		{name: "mapped-private", address: "[::ffff:10.0.0.8]:993"},
		{name: "public-v4", address: "198.51.100.8:993", allowed: true},
		{name: "public-v6", address: "[2001:db8::8]:993", allowed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAddress(tt.address, 993, false)
			if tt.allowed && err != nil {
				t.Fatalf("ValidateAddress(%q): %v", tt.address, err)
			}
			if !tt.allowed && err == nil {
				t.Fatalf("ValidateAddress(%q) allowed a reserved endpoint", tt.address)
			}
		})
	}
}

func TestAllowedAddressesFiltersMixedAnswersAndDeduplicates(t *testing.T) {
	ips := []net.IPAddr{
		{IP: net.ParseIP("127.0.0.1")},
		{IP: net.ParseIP("::ffff:10.0.0.8")},
		{IP: net.ParseIP("198.51.100.8")},
		{IP: net.ParseIP("2001:db8::8")},
		{IP: net.ParseIP("198.51.100.8")},
	}
	got := allowedAddresses(ips, 993, false)
	want := []string{"198.51.100.8:993", "[2001:db8::8]:993"}
	if len(got) != len(want) {
		t.Fatalf("allowedAddresses=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("allowedAddresses[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestPinnedAddressIsValidatedWithoutResolvingHostname(t *testing.T) {
	if _, err := DialContext(context.Background(), "provider.example", 993, "127.0.0.1:993", false); err == nil {
		t.Fatal("disallowed pinned address was dialed")
	}
}

func TestCheckAndPinRejectsReservedResolutionBeforeDial(t *testing.T) {
	address, err := CheckAndPin(context.Background(), "127.0.0.1", 993, false)
	if err == nil {
		t.Fatalf("CheckAndPin returned address %q for loopback", address)
	}
}
