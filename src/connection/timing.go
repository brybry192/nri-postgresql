package connection

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConnectionTiming holds DNS, TCP, and TLS timing for a new database connection.
// Values are captured inside the pgx DialFunc (DNS + TCP) and via a TLS
// VerifyConnection callback (TLS handshake). Both fire on the first real query
// issued against the connection — no extra Ping is used.
type ConnectionTiming struct {
	DNSLookupMs    float64
	TCPConnectMs   float64
	TLSHandshakeMs float64

	// dialCompleteAt records when the TCP dial finished. Used by the TLS
	// VerifyConnection callback to compute handshake duration. Not exported.
	dialCompleteAt time.Time
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
		timing.dialCompleteAt = time.Now()
		return conn, err
	}
}

// attachTimingDialFunc sets the DialFunc on a pgx ConnConfig to capture DNS and TCP timing.
// If TLS is configured, it also chains a VerifyConnection callback to measure the TLS
// handshake duration (time from TCP completion to TLS handshake finish, including the
// PostgreSQL SSLRequest negotiation).
func attachTimingDialFunc(config *pgx.ConnConfig, timing *ConnectionTiming) {
	config.Config.DialFunc = timingDialFunc(timing)

	if config.Config.TLSConfig != nil {
		attachTLSTimingCallback(config.Config.TLSConfig, timing)
	}
}

// attachTLSTimingCallback chains a VerifyConnection callback onto the TLS config that
// records the time between TCP dial completion and TLS handshake finish. Any existing
// VerifyConnection callback is preserved and called after timing is recorded.
func attachTLSTimingCallback(tlsConfig *tls.Config, timing *ConnectionTiming) {
	origVerify := tlsConfig.VerifyConnection
	tlsConfig.VerifyConnection = func(cs tls.ConnectionState) error {
		if !timing.dialCompleteAt.IsZero() {
			timing.TLSHandshakeMs = msec(time.Since(timing.dialCompleteAt))
		}
		if origVerify != nil {
			return origVerify(cs)
		}
		return nil
	}
}

func msec(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
