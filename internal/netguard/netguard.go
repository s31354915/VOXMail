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
	host = strings.TrimSpace(host)
	if host == "" || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid outbound endpoint")
	}
	if address != "" {
		endpointHost, endpointPort, err := net.SplitHostPort(address)
		if err != nil || endpointHost == "" || endpointPort != strconv.Itoa(port) {
			return nil, fmt.Errorf("invalid validated outbound address")
		}
		ip := net.ParseIP(endpointHost)
		if ip == nil || (!allowPrivate && blocked(ip)) {
			return nil, fmt.Errorf("outbound address is not allowed")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, resolved := range ips {
		if !allowPrivate && blocked(resolved.IP) {
			continue
		}
		conn, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(resolved.IP.String(), strconv.Itoa(port)))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("host resolves only to disallowed outbound addresses")
}

func blocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || isCGNAT(ip)
}

func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 0x40
}
