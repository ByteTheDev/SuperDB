package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"superdb/internal/engine"
)

// selfSigned writes a fresh self-signed certificate + key to a temp dir.
// The CA basic constraints let tests trust the cert directly as a root.
func selfSigned(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	cf, _ := os.Create(certFile)
	_ = pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = cf.Close()
	kf, _ := os.Create(keyFile)
	_ = pem.Encode(kf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	_ = kf.Close()
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestServerTLSConfigPairValidation(t *testing.T) {
	certFile, keyFile := selfSigned(t)
	if cfg, err := ServerTLSConfig("", ""); cfg != nil || err != nil {
		t.Fatalf("empty pair must disable TLS: cfg=%v err=%v", cfg, err)
	}
	if _, err := ServerTLSConfig(certFile, ""); err == nil {
		t.Fatal("cert without key must fail")
	}
	if _, err := ServerTLSConfig("", keyFile); err == nil {
		t.Fatal("key without cert must fail")
	}
	cfg, err := ServerTLSConfig(certFile, keyFile)
	if err != nil || cfg == nil || len(cfg.Certificates) != 1 {
		t.Fatalf("valid pair must configure TLS: cfg=%v err=%v", cfg, err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
}

func TestClientTLSConfigCAFile(t *testing.T) {
	if _, err := ClientTLSConfig(filepath.Join(t.TempDir(), "missing.pem"), false); err == nil {
		t.Fatal("missing CA file must fail")
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ClientTLSConfig(bad, false); err == nil {
		t.Fatal("PEM file without certificates must fail")
	}
	certFile, _ := selfSigned(t)
	cfg, err := ClientTLSConfig(certFile, false)
	if err != nil || cfg == nil || cfg.RootCAs == nil {
		t.Fatalf("CA file must build a root pool: cfg=%v err=%v", cfg, err)
	}
	cfg, err = ClientTLSConfig("", true)
	if err != nil || cfg == nil || !cfg.InsecureSkipVerify {
		t.Fatalf("skip-verify must be honored: cfg=%v err=%v", cfg, err)
	}
}

// serveTLS runs Handle on a TLS listener until it is closed.
func serveTLS(t *testing.T, ln net.Listener, db *engine.Database) {
	t.Helper()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go Handle(c, db, "memory", t.TempDir())
		}
	}()
}

func TestTLSListenerEndToEnd(t *testing.T) {
	certFile, keyFile := selfSigned(t)
	serverCfg, err := ServerTLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	db := engine.New()
	serveTLS(t, ln, db)
	addr := ln.Addr().String()

	// Trusted CA: query + ping over TLS work end to end.
	clientCfg, err := ClientTLSConfig(certFile, false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Dial(addr, 2*time.Second, clientCfg)
	if err != nil {
		t.Fatalf("TLS dial with server cert as CA: %v", err)
	}
	out := sendSQL(t, c, map[string]any{"sql": "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)"})
	if out["error"] != nil {
		t.Fatalf("query over TLS failed: %v", out)
	}
	out = sendSQL(t, c, map[string]any{"sql": "INSERT INTO users VALUES (1, 'Ada')"})
	if out["error"] != nil {
		t.Fatalf("insert over TLS failed: %v", out)
	}
	c.Close()
	if err := Ping(addr, "", 2*time.Second, clientCfg); err != nil {
		t.Fatalf("TLS ping: %v", err)
	}
	res, err := db.Exec("SELECT name FROM users WHERE id = 1")
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "Ada" {
		t.Fatalf("TLS write missing: %+v %v", res, err)
	}

	// A certificate the server does not present must not verify.
	otherCert, _ := selfSigned(t)
	wrongCfg, err := ClientTLSConfig(otherCert, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(addr, 2*time.Second, wrongCfg); err == nil {
		t.Fatal("untrusted CA must fail certificate verification")
	}

	// Plaintext against a TLS listener can never reach SQL.
	if err := Ping(addr, "", 2*time.Second, nil); err == nil {
		t.Fatal("plaintext ping must fail against a TLS listener")
	}
}

func TestDialPlaintextUnchanged(t *testing.T) {
	// nil config keeps the plain-TCP dial path.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()
	conn, err := Dial(ln.Addr().String(), 2*time.Second, nil)
	if err != nil {
		t.Fatalf("plaintext dial: %v", err)
	}
	conn.Close()
}
