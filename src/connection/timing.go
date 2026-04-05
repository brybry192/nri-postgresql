package connection

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConnectionTiming holds DNS and TCP timing for a new database connection.
// Both values are captured inside the pgx DialFunc, which fires on the first real
// query issued against the connection — no extra Ping is used. When
// ENABLE_AVAILABILITY_CHECK is true the DialFunc fires during that query, giving
// the timing breakdown for the same connection that the availability check validates.
type ConnectionTiming struct {
	DNSLookupMs  float64
	TCPConnectMs float64
}

// timingDialFunc returns a pgx DialFunc that measures DNS resolution and TCP connection time,
// storing results in the provided ConnectionTiming. It is attached to pgconn.Config.DialFunc
// before opening the connection.
func timingDialFunc(timing *ConnectionTiming) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid address %q: %w", addr, err)
		}

		// DNS resolution
		resolver := net.DefaultResolver
		dnsStart := time.Now()
		addrs, err := resolver.LookupHost(ctx, host)
		timing.DNSLookupMs = msec(time.Since(dnsStart))
		if err != nil {
			return nil, fmt.Errorf("dns lookup for %q failed: %w", host, err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("dns returned no addresses for %q", host)
		}

		// TCP connection (use first resolved address)
		tcpAddr := net.JoinHostPort(addrs[0], port)
		dialer := &net.Dialer{}
		tcpStart := time.Now()
		conn, err := dialer.DialContext(ctx, network, tcpAddr)
		timing.TCPConnectMs = msec(time.Since(tcpStart))
		return conn, err
	}
}

// attachTimingDialFunc sets the DialFunc on a pgx ConnConfig to capture DNS and TCP timing.
func attachTimingDialFunc(config *pgx.ConnConfig, timing *ConnectionTiming) {
	config.Config.DialFunc = timingDialFunc(timing)
}

func msec(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
