package auth

import (
	"context"
	"net"
	"net/http"
	"net/netip"
)

type clientIPKey struct{}

// ClientIPMiddleware records the caller's address so that SignIn, which sees
// only a context, can attribute a session to an origin.
//
// It reads RemoteAddr, which chi's RealIP middleware has already rewritten from
// X-Forwarded-For when present. That is only trustworthy behind a proxy you
// control — on a homelab reverse proxy it is, and directly exposed it cannot be
// spoofed into anything worse than a misleading audit field.
func ClientIPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if addr, ok := parseClientIP(r.RemoteAddr); ok {
			r = r.WithContext(WithClientIP(r.Context(), addr))
		}
		next.ServeHTTP(w, r)
	})
}

// WithClientIP attaches a caller address to ctx.
func WithClientIP(ctx context.Context, addr netip.Addr) context.Context {
	return context.WithValue(ctx, clientIPKey{}, addr)
}

func parseClientIP(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// RealIP leaves a bare address with no port.
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// ClientIPFrom returns the caller address recorded by ClientIPMiddleware.
func ClientIPFrom(ctx context.Context) (netip.Addr, bool) {
	addr, ok := ctx.Value(clientIPKey{}).(netip.Addr)
	return addr, ok
}
