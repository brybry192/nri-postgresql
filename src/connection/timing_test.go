package connection

import (
	"context"
	"crypto/tls"
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

// TestTimingDialFunc_DialCompleteAt verifies that dialCompleteAt is set after a
// successful TCP dial, enabling the TLS timing callback to compute handshake duration.
func TestTimingDialFunc_DialCompleteAt(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	timing := &ConnectionTiming{}
	dialFn := timingDialFunc(timing)
	conn, err := dialFn(context.Background(), "tcp", ln.Addr().String())
	require.NoError(t, err)
	conn.Close()

	assert.False(t, timing.dialCompleteAt.IsZero(), "dialCompleteAt should be set after successful dial")
}

// TestAttachTLSTimingCallback verifies that the VerifyConnection callback records
// TLS handshake duration and chains any existing callback.
func TestAttachTLSTimingCallback(t *testing.T) {
	timing := &ConnectionTiming{}
	timing.dialCompleteAt = time.Now().Add(-50 * time.Millisecond)

	existingCalled := false
	tlsConfig := &tls.Config{
		VerifyConnection: func(cs tls.ConnectionState) error {
			existingCalled = true
			return nil
		},
	}

	attachTLSTimingCallback(tlsConfig, timing)

	// Simulate the callback firing at the end of a TLS handshake.
	err := tlsConfig.VerifyConnection(tls.ConnectionState{})
	assert.NoError(t, err)
	assert.True(t, existingCalled, "original VerifyConnection callback should be chained")
	assert.Greater(t, timing.TLSHandshakeMs, 0.0, "TLS handshake time should be recorded")
}

// TestAttachTLSTimingCallback_NoExisting verifies the callback works when there
// is no pre-existing VerifyConnection on the TLS config.
func TestAttachTLSTimingCallback_NoExisting(t *testing.T) {
	timing := &ConnectionTiming{}
	timing.dialCompleteAt = time.Now().Add(-25 * time.Millisecond)

	tlsConfig := &tls.Config{}
	attachTLSTimingCallback(tlsConfig, timing)

	err := tlsConfig.VerifyConnection(tls.ConnectionState{})
	assert.NoError(t, err)
	assert.Greater(t, timing.TLSHandshakeMs, 0.0)
}

// TestAttachTLSTimingCallback_ZeroDialComplete verifies that TLS timing is not
// recorded if dialCompleteAt was never set (e.g. dial failed before TCP connected).
func TestAttachTLSTimingCallback_ZeroDialComplete(t *testing.T) {
	timing := &ConnectionTiming{}
	tlsConfig := &tls.Config{}
	attachTLSTimingCallback(tlsConfig, timing)

	err := tlsConfig.VerifyConnection(tls.ConnectionState{})
	assert.NoError(t, err)
	assert.Equal(t, 0.0, timing.TLSHandshakeMs, "should not record TLS time when dial never completed")
}
