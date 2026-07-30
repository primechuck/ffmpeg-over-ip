package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/steelbrain/ffmpeg-over-ip/internal/config"
)

// dialWithFailover tries addresses in order, 5s timeout each. No shuffle, no hedged, minimal.
func dialWithFailover(ctx context.Context, addrs []string) (net.Conn, string, error) {
	if len(addrs) == 0 {
		return nil, "", fmt.Errorf("no server address configured")
	}
	var lastErr error
	for _, addr := range addrs {
		network, target := config.ParseAddress(addr)
		d := net.Dialer{Timeout: 5 * time.Second}
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := d.DialContext(c, network, target)
		cancel()
		if err == nil {
			return conn, addr, nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}
