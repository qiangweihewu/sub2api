package dnscache

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// A failed cached address must not hide another reachable address for the host.
func TestDialContextFallsBackToReachableAddress(t *testing.T) {
	for _, failedIP := range []string{"::1", "127.0.0.2"} {
		t.Run(failedIP, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			resolver := resolverWithAddresses("127.0.0.1", failedIP)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			conn, err := resolver.DialContext(ctx, "tcp", net.JoinHostPort("cached.invalid", port))
			if err != nil {
				t.Fatalf("reachable IPv4 was not tried after %s failed: %v", failedIP, err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			if conn.RemoteAddr().String() != listener.Addr().String() {
				t.Fatalf("connected to %s, want %s", conn.RemoteAddr(), listener.Addr())
			}
		})
	}
}

func TestDialContextHonorsNetworkFamily(t *testing.T) {
	for _, tc := range []struct {
		network string
		ip      string
		otherIP string
	}{
		{network: "tcp4", ip: "127.0.0.1", otherIP: "::1"},
		{network: "tcp6", ip: "::1", otherIP: "127.0.0.1"},
	} {
		t.Run(tc.network, func(t *testing.T) {
			listener, err := net.Listen(tc.network, net.JoinHostPort(tc.ip, "0"))
			if err != nil {
				if tc.network == "tcp6" {
					t.Skipf("IPv6 loopback unavailable: %v", err)
				}
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			resolver := resolverWithAddresses(tc.ip, tc.otherIP)
			conn, err := resolver.DialContext(context.Background(), tc.network, net.JoinHostPort("cached.invalid", port))
			if err != nil {
				t.Fatalf("address from requested family was not tried: %v", err)
			}
			_ = conn.Close()
		})
	}
}

func TestDialContextRejectsInvalidCachedAddresses(t *testing.T) {
	resolver := resolverWithAddresses("not-an-ip")
	conn, err := resolver.DialContext(context.Background(), "tcp", "cached.invalid:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("unexpected connection for invalid DNS answer")
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Fatalf("want DNS not-found error, got %v", err)
	}
}

func TestDialContextHonorsCanceledContext(t *testing.T) {
	resolver := resolverWithAddresses("127.0.0.1", "::1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := resolver.DialContext(ctx, "tcp", "cached.invalid:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("unexpected connection after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation error, got %v", err)
	}
}

func resolverWithAddresses(addrs ...string) *Resolver {
	resolver := New(time.Minute)
	resolver.cache.Store("cached.invalid", &cacheEntry{
		addrs:     addrs,
		expiresAt: time.Now().Add(time.Minute).UnixNano(),
	})
	return resolver
}
