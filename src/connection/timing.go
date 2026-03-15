package connection

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConnectionTiming holds measured timing for each phase of a new database connection.
// DNS and TCP times are captured inside the pgx DialFunc (the actual connection, not a probe).
// TotalConnectMs covers the full authenticated session: DNS + TCP + TLS + auth.
// TLSAndAuthMs is derived: TotalConnectMs - DNSLookupMs - TCPConnectMs.
type ConnectionTiming struct {
	DNSLookupMs    float64
	TCPConnectMs   float64
	TotalConnectMs float64
}

// TLSAndAuthMs returns the portion of connect time spent in TLS handshake and authentication.
// This is a derived value because pgx handles TLS internally after the TCP conn is returned
// from DialFunc — the two phases cannot be measured separately without wrapping net.Conn.
func (ct *ConnectionTiming) TLSAndAuthMs() float64 {
	v := ct.TotalConnectMs - ct.DNSLookupMs - ct.TCPConnectMs
	if v < 0 {
		return 0
	}
	return v
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
		if err != nil {
			return nil, err
		}

		return conn, nil
	}
}

// attachTimingDialFunc sets the DialFunc on a pgx ConnConfig to capture DNS and TCP timing.
func attachTimingDialFunc(config *pgx.ConnConfig, timing *ConnectionTiming) {
	config.Config.DialFunc = timingDialFunc(timing)
}

func msec(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
