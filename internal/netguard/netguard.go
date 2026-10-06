// Package netguard provides outbound dialing for user-configured providers.
// Mail servers are entered through the web console, so dialing them directly
// must not allow a user to probe loopback, private, metadata, or other
// non-public address ranges in a multi-user deployment.
package netguard

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
)

func DialContext(ctx context.Context, host string, port int, address string, allowPrivate bool) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if address != "" {
		return DialAddressContext(ctx, address, allowPrivate)
	}
	addresses, err := PublicAddresses(ctx, host, port, allowPrivate)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, resolved := range addresses {
		conn, dialErr := DialAddressContext(ctx, resolved, allowPrivate)
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("host resolves only to disallowed outbound addresses")
}

// PublicAddresses resolves host once and returns only addresses allowed by
// the outbound policy. Callers that need to prevent DNS rebinding must retain
// one returned address and use DialAddressContext with that exact value.
func PublicAddresses(ctx context.Context, host string, port int, allowPrivate bool) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	host = strings.TrimSpace(host)
	if host == "" || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid outbound endpoint")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	addresses := allowedAddresses(ips, port, allowPrivate)
	if len(addresses) == 0 {
		return nil, fmt.Errorf("host resolves only to disallowed outbound addresses")
	}
	return addresses, nil
}

// CheckAndPin resolves host once, verifies that an allowed numeric endpoint
// is reachable, and returns that exact endpoint for the subsequent protocol
// connection. Callers must pass the returned value as an explicit address;
// passing host again would reintroduce a DNS-rebinding window.
func CheckAndPin(ctx context.Context, host string, port int, allowPrivate bool) (string, error) {
	addresses, err := PublicAddresses(ctx, host, port, allowPrivate)
	if err != nil {
		return "", err
	}
	var lastErr error
	for _, address := range addresses {
		conn, dialErr := DialAddressContext(ctx, address, allowPrivate)
		if dialErr == nil {
			_ = conn.Close()
			return address, nil
		}
		lastErr = dialErr
		if ctx != nil && ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("no allowed outbound address was reachable")
}

// ValidateAddress checks an already-resolved numeric endpoint without doing
// DNS. It is used by the mbsync tunnel helper and generated configuration so
// a later hostname resolution cannot bypass the same policy.
func ValidateAddress(address string, port int, allowPrivate bool) error {
	endpointHost, endpointPort, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil || endpointHost == "" || endpointPort != strconv.Itoa(port) {
		return fmt.Errorf("invalid validated outbound address")
	}
	ip := net.ParseIP(endpointHost)
	if ip == nil || (!allowPrivate && blocked(ip)) {
		return fmt.Errorf("outbound address is not allowed")
	}
	return nil
}

// DialAddressContext dials a numeric address after validating it. It never
// performs a hostname lookup.
func DialAddressContext(ctx context.Context, address string, allowPrivate bool) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	_, portText, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return nil, fmt.Errorf("invalid validated outbound address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid validated outbound address")
	}
	if err := ValidateAddress(address, port, allowPrivate); err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}

func allowedAddresses(ips []net.IPAddr, port int, allowPrivate bool) []string {
	addresses := make([]string, 0, len(ips))
	seen := make(map[string]struct{}, len(ips))
	for _, resolved := range ips {
		if resolved.IP == nil || (!allowPrivate && blocked(resolved.IP)) {
			continue
		}
		address := net.JoinHostPort(resolved.IP.String(), strconv.Itoa(port))
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	return addresses
}

func blocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || isCGNAT(ip)
}

func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 0x40
}
