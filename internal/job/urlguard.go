package job

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"coverletter/internal/apperror"
)

// ValidateJobURL parses and sanity-checks a user-supplied job URL.
//
// This covers what can be judged from the URL alone: scheme, presence of a
// host, and syntactically valid credentials-free authority. Whether the host
// actually resolves to a public address is decided later by the guard, because
// resolution results go stale (see ResolveGuard).
func ValidateJobURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, apperror.Validation(apperror.CodeInvalidJobInput, "Job URL must not be empty.")
	}
	if len(trimmed) > 2048 {
		return nil, apperror.Validation(apperror.CodeInvalidJobInput, "Job URL is too long.")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, apperror.Wrap(err, apperror.CodeInvalidJobInput, "Job URL is not a valid URL.", 400)
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		// Anything else (file, gopher, ftp, javascript, ...) is rejected rather
		// than silently coerced.
		return nil, apperror.Validation(apperror.CodeInvalidJobInput, "Job URL must use http or https.")
	}

	if parsed.Host == "" {
		return nil, apperror.Validation(apperror.CodeInvalidJobInput, "Job URL must include a host.")
	}
	if parsed.User != nil {
		// Credentials in a URL are a classic phishing vector and have no
		// legitimate use for a job posting.
		return nil, apperror.Validation(apperror.CodeInvalidJobInput, "Job URL must not contain credentials.")
	}

	host := parsed.Hostname()
	if host == "" {
		return nil, apperror.Validation(apperror.CodeInvalidJobInput, "Job URL must include a host.")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return nil, apperror.Validation(apperror.CodeInvalidJobInput, "Job URL contains invalid characters.")
	}
	return parsed, nil
}

// resolveGuard decides whether a hostname may be contacted.
//
// The check runs against the *resolved* IP addresses rather than the hostname,
// because that is what the HTTP client will actually connect to. It is applied
// twice: once before the request (fast failure, clear error) and once inside
// the dialer (re-validating the address that the connection is about to use).
// The dial-time check is what closes the DNS-rebinding window, where a name
// resolves to a public address during validation and to 127.0.0.1 a moment
// later.
type resolveGuard struct {
	resolver     ipResolver
	allowPrivate bool
}

// ipResolver is the slice of *net.Resolver this package needs. Depending on
// the narrow interface rather than the concrete type is what lets the SSRF
// rules be tested against scripted DNS answers instead of live lookups.
type ipResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

func newResolveGuard(resolver ipResolver, allowPrivate bool) *resolveGuard {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &resolveGuard{resolver: resolver, allowPrivate: allowPrivate}
}

// hostnames that never legitimately point at a public job posting.
var blockedHostSuffixes = []string{".localhost", ".local", ".internal", ".home.arpa"}

// checkHost resolves host and rejects it if any resolved address is not routable
// on the public internet.
func (g *resolveGuard) checkHost(ctx context.Context, host string) error {
	if g.allowPrivate {
		return nil
	}

	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" {
		return unsafeHostError(host, "hostname resolves to the loopback interface")
	}
	for _, suffix := range blockedHostSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return unsafeHostError(host, "hostname uses an internal-only suffix")
		}
	}

	// Strip an IPv6 zone ("fe80::1%en0") before checking the address itself.
	if zoneStripped, _, ok := strings.Cut(host, "%"); ok {
		host = zoneStripped
	}

	addrs, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return apperror.Wrap(err, apperror.CodeJobExtraction, "Could not resolve the job URL host.", 502)
	}
	if len(addrs) == 0 {
		return unsafeHostError(host, "hostname did not resolve to any address")
	}
	for _, addr := range addrs {
		if reason, blocked := isBlockedIP(addr.IP); blocked {
			return unsafeHostError(host, fmt.Sprintf("hostname resolves to a non-public address (%s: %s)", addr.IP, reason))
		}
	}
	return nil
}

// dialContext is the guard's net.Dialer wrapper. It re-checks the address at
// connection time, which is the authoritative SSRF defence.
func (g *resolveGuard) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("job: parse dial address %q: %w", address, err)
	}

	if g.allowPrivate {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, address)
	}

	// The address came from a URL we already validated; resolve it ourselves so
	// the check below applies to every candidate IP.
	ips, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, apperror.Wrap(err, apperror.CodeUnsafeURL, "Could not resolve the job URL host.", 400)
	}
	if len(ips) == 0 {
		return nil, apperror.New(apperror.CodeUnsafeURL, "Job URL host did not resolve to any address.", 400)
	}

	var dialer net.Dialer
	var lastErr error
	for _, ip := range ips {
		if reason, blocked := isBlockedIP(ip.IP); blocked {
			lastErr = fmt.Errorf("job: refusing to connect to %s (%s)", ip.IP, reason)
			continue
		}
		target := net.JoinHostPort(ip.IP.String(), port)
		conn, err := dialer.DialContext(ctx, network, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("job: no usable address for %q", host)
	}
	return nil, lastErr
}

// isBlockedIP reports whether an address must not be contacted, along with a
// short reason for logging. It covers loopback, private, link-local, carrier
// NAT, multicast, documentation and IPv6 unique-local ranges.
func isBlockedIP(ip net.IP) (reason string, blocked bool) {
	if ip == nil {
		return "address is nil", true
	}
	// Normalise IPv4-mapped IPv6 addresses (::ffff:10.0.0.1) to IPv4 first,
	// otherwise they would slip past the IPv4 range checks.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	switch {
	case ip.IsUnspecified():
		return "unspecified address", true
	case ip.IsLoopback():
		return "loopback address", true
	case ip.IsPrivate():
		return "private address", true
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "link-local address", true
	case ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return "multicast address", true
	}

	if v4 := ip.To4(); v4 != nil {
		switch {
		case inCIDR(v4, "100.64.0.0", 10): // RFC 6598 carrier-grade NAT
			return "carrier-grade NAT address", true
		case inCIDR(v4, "192.0.0.0", 24): // RFC 6890 IETF protocol assignments
			return "reserved address", true
		case inCIDR(v4, "198.18.0.0", 15): // RFC 2544 benchmarking
			return "benchmarking address", true
		case v4[0] == 0 || v4[0] >= 240: // "this network" and reserved space
			return "reserved address", true
		}
		return "", false
	}

	// IPv6: fc00::/7 unique-local addresses are the private range equivalent of
	// RFC 1918 and are not covered by net.IP.IsPrivate on all platforms.
	if len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc {
		return "IPv6 unique-local address", true
	}
	return "", false
}

// inCIDR matches an IPv4 address against a prefix given as a dotted address and
// bit length. The set of ranges used here is fixed, so a failed parse in this
// helper is a programming error and falls back to "not blocked" rather than
// panicking in a request path.
func inCIDR(ip net.IP, network string, bits int) bool {
	base := net.ParseIP(network).To4()
	if base == nil {
		return false
	}
	mask := net.CIDRMask(bits, 32)
	return base.Mask(mask).Equal(ip.Mask(mask))
}

// unsafeHostError builds an SSRF rejection. The specific reason goes into the
// wrapped cause for logs; the client only ever sees a generic message, because
// the reason can disclose internal network topology.
func unsafeHostError(host, reason string) error {
	cause := fmt.Errorf("blocked job URL host %q: %s", host, reason)
	return apperror.Wrap(cause, apperror.CodeUnsafeURL, "Job URL host is not allowed.", 400)
}
