package superdb

import "testing"

func TestParseBasic(t *testing.T) {
	cfg, err := ParseURL("superdb://user:password@localhost:7432/mydb")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "localhost" || cfg.Port != 7432 || cfg.Username != "user" ||
		cfg.Password != "password" || cfg.Database != "mydb" {
		t.Fatalf("%+v", cfg)
	}
	if cfg.TLS {
		t.Fatal("tls should default off")
	}
}

func TestParseProxyHost(t *testing.T) {
	cfg, err := ParseURL("superdb://abc.proxy.rlwy.net:18432/mydb")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "abc.proxy.rlwy.net" || cfg.Port != 18432 {
		t.Fatalf("%+v", cfg)
	}
}

func TestParseIPv6(t *testing.T) {
	cfg, err := ParseURL("superdb://user@[::1]:7432/main")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "::1" || cfg.Port != 7432 {
		t.Fatalf("%+v", cfg)
	}
	if addr := cfg.Address(); addr != "[::1]:7432" {
		t.Fatalf("addr %q", addr)
	}
}

func TestParseURLEncoding(t *testing.T) {
	cfg, err := ParseURL("superdb://us%40er:p%40ss%3Aword@db.example.com:7432/my%20db?tls=true&timeout=5s")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Username != "us@er" || cfg.Password != "p@ss:word" || cfg.Database != "my db" {
		t.Fatalf("%+v", cfg)
	}
	if !cfg.TLS || cfg.TLSInsecure {
		t.Fatalf("tls %+v", cfg)
	}
	if cfg.ConnectTimeout.Seconds() != 5 {
		t.Fatalf("timeout %+v", cfg.ConnectTimeout)
	}
}

func TestParseURLDoesNotDoubleDecode(t *testing.T) {
	// A password whose literal value is "p%20ss" is encoded %2520. Decoding
	// twice would corrupt it to "p ss"; exactly once yields "p%20ss".
	cfg, err := ParseURL("superdb://u:p%2520ss@h:1/db%25x")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != "p%20ss" {
		t.Fatalf("password double-decoded: %q", cfg.Password)
	}
	if cfg.Database != "db%x" {
		t.Fatalf("database double-decoded: %q", cfg.Database)
	}
}

func TestParseTLSModes(t *testing.T) {
	for _, raw := range []string{"superdb://h:1/db?tls=insecure", "superdb://h:1/db?tls_skip_verify=true"} {
		cfg, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.TLS || !cfg.TLSInsecure {
			t.Fatalf("%s -> %+v", raw, cfg)
		}
	}
}

func TestParseMalformed(t *testing.T) {
	for _, raw := range []string{
		"",
		"http://localhost:7432/db",
		"superdb://",
		"superdb://user:pass@[::1/db",
		"superdb://h:99999/db",
		"superdb://h/db?tls=bogus",
		"superdb://h/db?timeout=bogus",
	} {
		if _, err := ParseURL(raw); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}
