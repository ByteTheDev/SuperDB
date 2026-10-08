package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// ServerTLSConfig loads a certificate/key pair for the local server
// listener. Both paths must be set together; both empty returns a nil
// config so callers stay on plaintext. Mirrors the hosted server's TLS
// policy (TLS 1.2 minimum).
func ServerTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	if (certFile != "") != (keyFile != "") {
		return nil, errors.New("tls-cert and tls-key must be set together")
	}
	if certFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load tls: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// ClientTLSConfig builds a client-side config for superdb-cli and status
// probes. caFile, when set, is a PEM bundle added to the trust roots —
// the usual way to trust a self-signed server certificate; otherwise the
// system roots apply. insecureSkipVerify disables verification entirely
// and is an explicit opt-in.
func ClientTLSConfig(caFile string, insecureSkipVerify bool) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: insecureSkipVerify, //nolint:gosec // explicit opt-in flag
	}
	if caFile == "" {
		return cfg, nil
	}
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read tls-ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("tls-ca %s contains no PEM certificates", caFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// Dial opens a client connection to addr: plaintext when tlsCfg is nil,
// otherwise TLS with the handshake bounded by timeout. ServerName
// defaults to the host part of addr when the config leaves it empty.
func Dial(addr string, timeout time.Duration, tlsCfg *tls.Config) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if tlsCfg == nil {
		return net.DialTimeout("tcp", addr, timeout)
	}
	cfg := tlsCfg.Clone()
	if cfg.ServerName == "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("tls dial %s: %w", addr, err)
		}
		cfg.ServerName = host
	}
	return tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", addr, cfg)
}
