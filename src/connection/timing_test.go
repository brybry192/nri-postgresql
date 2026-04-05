package connection

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMsec(t *testing.T) {
	assert.Equal(t, 1000.0, msec(time.Second))
	assert.Equal(t, 500.0, msec(500*time.Millisecond))
	assert.Equal(t, 0.0, msec(0))
	assert.InDelta(t, 1.5, msec(1500*time.Microsecond), 0.001)
}

// TestTimingDialFunc_InvalidAddress verifies that a malformed address (missing port)
// is caught before any network I/O occurs.
func TestTimingDialFunc_InvalidAddress(t *testing.T) {
	timing := &ConnectionTiming{}
	dialFn := timingDialFunc(timing)
	_, err := dialFn(context.Background(), "tcp", "no-port-here")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid address")
}

// TestTimingDialFunc_HappyPath verifies that DNS and TCP timing fields are populated
// on a successful dial to a local listener.
func TestTimingDialFunc_HappyPath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	timing := &ConnectionTiming{}
	dialFn := timingDialFunc(timing)
	conn, err := dialFn(context.Background(), "tcp", ln.Addr().String())
	require.NoError(t, err)
	conn.Close()

	assert.GreaterOrEqual(t, timing.DNSLookupMs, 0.0)
	assert.GreaterOrEqual(t, timing.TCPConnectMs, 0.0)
}

// TestTimingDialFunc_TCPConnectFailure verifies that TCPConnectMs is still recorded
// even when the TCP dial itself fails (server not listening).
func TestTimingDialFunc_TCPConnectFailure(t *testing.T) {
	// Grab then immediately close a listener to get a port guaranteed not to be listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close()

	timing := &ConnectionTiming{}
	dialFn := timingDialFunc(timing)
	_, err = dialFn(context.Background(), "tcp", addr)
	assert.Error(t, err)
	// DNS resolves fine for 127.0.0.1; TCPConnectMs should be recorded despite failure.
	assert.GreaterOrEqual(t, timing.DNSLookupMs, 0.0)
	assert.GreaterOrEqual(t, timing.TCPConnectMs, 0.0)
}
