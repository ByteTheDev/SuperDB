// Package superdb is the first-party Go client for hosted SuperDB servers
// plus connection-string parsing. It speaks the SDB1 framed protocol and
// never concatenates user values into SQL: params travel separately and
// are bound server-side with the shared safe binder.
package superdb

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ConnConfig is the parsed form of a superdb:// URL.
type ConnConfig struct {
	Host           string
	Port           int
	Username       string
	Password       string
	Database       string
	TLS            bool
	TLSInsecure    bool
	ConnectTimeout time.Duration
}

// ParseURL parses superdb://username:password@host:port/database?tls=true.
// IPv6 hosts, URL-escaped credentials, and query params are all supported.
func ParseURL(raw string) (ConnConfig, error) {
	cfg := ConnConfig{Port: 7432, Database: "main", ConnectTimeout: 10 * time.Second}
	if raw == "" {
		return cfg, fmt.Errorf("empty connection string")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return cfg, fmt.Errorf("invalid connection string: %w", err)
	}
	if u.Scheme != "superdb" {
		return cfg, fmt.Errorf("invalid scheme %q: want superdb://", u.Scheme)
	}
	if u.Host == "" {
		return cfg, fmt.Errorf("connection string requires a host (superdb://user:pass@host:7432/db)")
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		// No explicit port: u.Host is bare hostname/IP (handles IPv6 ::1 without brackets? require brackets).
		host = u.Host
		// Strip IPv6 brackets for storage; net.JoinHostPort re-adds them.
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
		// If it still contains a colon but SplitHostPort failed, it is malformed.
		if strings.Contains(host, ":") && !isIPLiteral(host) {
			return cfg, fmt.Errorf("invalid host %q: IPv6 addresses must use brackets, e.g. superdb://user@[::1]:7432/db", u.Host)
		}
	} else {
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
		if portStr != "" {
			p, err := strconv.Atoi(portStr)
			if err != nil || p <= 0 || p > 65535 {
				return cfg, fmt.Errorf("invalid port %q", portStr)
			}
			cfg.Port = p
		}
	}
	if host == "" {
		return cfg, fmt.Errorf("connection string requires a host")
	}
	cfg.Host = host
	// url.Parse already percent-decodes the userinfo and path exactly once;
	// decoding again would corrupt values that legitimately contain %XX
	// (e.g. a password encoded as %2520 -> "%20" -> " ").
	if u.User != nil {
		if name := u.User.Username(); name != "" {
			cfg.Username = name
		}
		if pw, ok := u.User.Password(); ok {
			cfg.Password = pw
		}
	}
	if strings.Trim(u.Path, "/") != "" {
		db := strings.TrimPrefix(u.Path, "/")
		db = strings.SplitN(db, "/", 2)[0]
		if db != "" {
			cfg.Database = db
		}
	}
	q := u.Query()
	switch strings.ToLower(q.Get("tls")) {
	case "", "false", "0", "disable", "disabled", "off":
	case "true", "1", "require", "required", "on":
		cfg.TLS = true
	case "insecure", "skip-verify", "skip_verify":
		cfg.TLS = true
		cfg.TLSInsecure = true
	default:
		return cfg, fmt.Errorf("invalid tls value %q: want true, false, or insecure", q.Get("tls"))
	}
	if v := q.Get("tls_skip_verify"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid tls_skip_verify value %q", v)
		}
		if b {
			cfg.TLS = true
			cfg.TLSInsecure = true
		}
	}
	if v := firstNonEmpty(q.Get("timeout"), q.Get("connect_timeout")); v != "" {
		d, err := parseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid timeout %q: %w", v, err)
		}
		cfg.ConnectTimeout = d
	}
	return cfg, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("want Go duration (e.g. 5s) or seconds")
}

func isIPLiteral(s string) bool {
	return net.ParseIP(s) != nil
}

// Address returns host:port with IPv6 bracketing handled.
func (c ConnConfig) Address() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }
