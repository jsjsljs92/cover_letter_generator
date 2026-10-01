package job

import (
	"context"
	"net"
	"testing"

	"coverletter/internal/apperror"
)

func TestValidateJobURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantErr string // empty means the URL is accepted
	}{
		{name: "https url", raw: "https://jobs.example.com/roles/1"},
		{name: "http url", raw: "http://jobs.example.com/roles/1"},
		{name: "url with query and fragment", raw: "https://example.com/job?id=42#top"},
		{name: "url with port", raw: "https://example.com:8443/job"},
		{name: "surrounding whitespace is trimmed", raw: "  https://example.com/job  "},

		{name: "empty", raw: "", wantErr: apperror.CodeInvalidJobInput},
		{name: "whitespace only", raw: "   ", wantErr: apperror.CodeInvalidJobInput},
		{name: "unsupported scheme file", raw: "file:///etc/passwd", wantErr: apperror.CodeInvalidJobInput},
		{name: "unsupported scheme javascript", raw: "javascript:alert(1)", wantErr: apperror.CodeInvalidJobInput},
		{name: "unsupported scheme gopher", raw: "gopher://example.com/", wantErr: apperror.CodeInvalidJobInput},
		{name: "scheme relative", raw: "//example.com/job", wantErr: apperror.CodeInvalidJobInput},
		{name: "no host", raw: "https:///job", wantErr: apperror.CodeInvalidJobInput},
		{name: "credentials in url", raw: "https://user:pass@example.com/job", wantErr: apperror.CodeInvalidJobInput},
		{name: "control characters", raw: "https://example.com/job\nHost: evil", wantErr: apperror.CodeInvalidJobInput},
		{name: "too long", raw: "https://example.com/" + string(make([]byte, 3000)), wantErr: apperror.CodeInvalidJobInput},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ValidateJobURL(tc.raw)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ValidateJobURL(%q) = %v, want no error", tc.raw, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("ValidateJobURL(%q) = nil, want error with code %s", tc.raw, tc.wantErr)
			case tc.wantErr != "" && !apperror.Is(err, tc.wantErr):
				t.Fatalf("ValidateJobURL(%q) = %v, want code %s", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestIsBlockedIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		ip          string
		wantBlocked bool
	}{
		{name: "public ipv4", ip: "93.184.216.34", wantBlocked: false},
		{name: "public ipv6", ip: "2606:2800:220:1:248:1893:25c8:1946", wantBlocked: false},

		{name: "loopback ipv4", ip: "127.0.0.1", wantBlocked: true},
		{name: "loopback alias", ip: "127.0.0.53", wantBlocked: true},
		{name: "loopback ipv6", ip: "::1", wantBlocked: true},
		{name: "private 10/8", ip: "10.0.0.5", wantBlocked: true},
		{name: "private 172.16/12", ip: "172.16.4.9", wantBlocked: true},
		{name: "private 192.168/16", ip: "192.168.1.1", wantBlocked: true},
		{name: "link local v4", ip: "169.254.169.254", wantBlocked: true}, // cloud metadata endpoint
		{name: "unspecified v4", ip: "0.0.0.0", wantBlocked: true},
		{name: "this-network 0/8", ip: "0.1.2.3", wantBlocked: true},
		{name: "carrier grade nat", ip: "100.64.0.1", wantBlocked: true},
		{name: "reserved 192.0.0/24", ip: "192.0.0.8", wantBlocked: true},
		{name: "benchmarking range", ip: "198.18.0.1", wantBlocked: true},
		{name: "multicast v4", ip: "224.0.0.1", wantBlocked: true},
		{name: "broadcast range", ip: "255.255.255.255", wantBlocked: true},

		{name: "ipv6 unique local", ip: "fd00::1", wantBlocked: true},
		{name: "ipv6 unique local fc00", ip: "fc00::1", wantBlocked: true},
		{name: "ipv6 link local", ip: "fe80::1", wantBlocked: true},
		{name: "ipv6 multicast", ip: "ff02::1", wantBlocked: true},

		// An IPv4-mapped IPv6 address must be judged as IPv4, or
		// "::ffff:127.0.0.1" would slip past the loopback check.
		{name: "ipv4-mapped loopback", ip: "::ffff:127.0.0.1", wantBlocked: true},
		{name: "ipv4-mapped private", ip: "::ffff:10.0.0.1", wantBlocked: true},
		{name: "ipv4-mapped public", ip: "::ffff:93.184.216.34", wantBlocked: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("net.ParseIP(%q) = nil, want a valid address", tc.ip)
			}
			_, blocked := isBlockedIP(ip)
			if blocked != tc.wantBlocked {
				t.Fatalf("isBlockedIP(%s) = %v, want %v", tc.ip, blocked, tc.wantBlocked)
			}
		})
	}
}

// fakeResolver lets the guard's DNS decisions be tested without touching the
// network.
type fakeResolver struct {
	addrs []net.IPAddr
	err   error
}

func (r fakeResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return r.addrs, r.err
}

func TestResolveGuardCheckHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		host      string
		addrs     []net.IPAddr
		wantErr   string // empty means allowed
		allowPriv bool
	}{
		{name: "public host", host: "jobs.example.com", addrs: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}},
		{name: "localhost by name", host: "localhost", addrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, wantErr: apperror.CodeUnsafeURL},
		{name: "internal suffix", host: "db.internal", addrs: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, wantErr: apperror.CodeUnsafeURL},
		{name: "localhost suffix", host: "app.localhost", addrs: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, wantErr: apperror.CodeUnsafeURL},
		{name: "resolves to loopback", host: "evil.example.com", addrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, wantErr: apperror.CodeUnsafeURL},
		{name: "resolves to metadata endpoint", host: "evil.example.com", addrs: []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, wantErr: apperror.CodeUnsafeURL},
		{
			// A host resolving to both a public and a private address is
			// rejected: an attacker controls which one the dialer picks.
			name:    "mixed public and private",
			host:    "rebind.example.com",
			addrs:   []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("10.0.0.1")}},
			wantErr: apperror.CodeUnsafeURL,
		},
		{name: "no addresses", host: "empty.example.com", addrs: nil, wantErr: apperror.CodeUnsafeURL},
		{
			name:      "private allowed when opted in",
			host:      "localhost",
			addrs:     []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}},
			allowPriv: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			guard := newResolveGuard(fakeResolver{addrs: tc.addrs}, tc.allowPriv)
			err := guard.checkHost(context.Background(), tc.host)

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("checkHost(%q) = %v, want allowed", tc.host, err)
			case tc.wantErr != "" && !apperror.Is(err, tc.wantErr):
				t.Fatalf("checkHost(%q) = %v, want code %s", tc.host, err, tc.wantErr)
			}
		})
	}
}

func TestResolveGuardCheckHostResolutionFailure(t *testing.T) {
	t.Parallel()

	guard := newResolveGuard(fakeResolver{err: context.DeadlineExceeded}, false)
	err := guard.checkHost(context.Background(), "slow.example.com")

	// A DNS failure is a transient upstream problem, not an SSRF rejection, so
	// it must be reported as a job extraction failure rather than a 400.
	if err == nil {
		t.Fatal("checkHost() = nil, want an error")
	}
	if apperror.Is(err, apperror.CodeUnsafeURL) {
		t.Fatalf("checkHost() = %v, want a resolution failure rather than an unsafe-URL rejection", err)
	}
	if !apperror.Is(err, apperror.CodeJobExtraction) {
		t.Fatalf("checkHost() = %v, want code %s", err, apperror.CodeJobExtraction)
	}
}
