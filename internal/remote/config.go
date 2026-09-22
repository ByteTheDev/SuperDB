package remote

import (
	"crypto/sha256"
	"crypto/subtle"
	"os"
	"strconv"
	"time"

	"superdb/internal/server"
)

// Config controls the hosted SuperDB network server.
type Config struct {
	Host            string
	Port            int
	DataDir         string
	Mode            string // memory | wal | snapshot
	Username        string
	Password        string
	TLSCertFile     string
	TLSKeyFile      string
	MaxConnections  int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	HealthAddr      string
	ClusterAddr     string
	AdvertiseAddr   string
	JoinAddrs       []string
	Region          string
	// Limits bounds per-query resource use and admission control
	// (timeouts, result size, inflight queries, transaction caps).
	// Zero fields disable each cap.
	Limits server.Limits
}

func defaults() Config {
	return Config{
		Host:            "127.0.0.1",
		Port:            7432,
		DataDir:         "./data",
		Mode:            "wal",
		MaxConnections:  128,
		ReadTimeout:     60 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     5 * time.Minute,
		ShutdownTimeout: 10 * time.Second,
	}
}

// LoadConfig merges explicit flags with environment variables.
// Explicit non-zero flag values win; otherwise env is consulted:
// SUPERDB_HOST, SUPERDB_PORT (or PORT), SUPERDB_DATA_DIR, SUPERDB_USERNAME,
// SUPERDB_PASSWORD, SUPERDB_TLS_CERT, SUPERDB_TLS_KEY,
// SUPERDB_MAX_CONNECTIONS, SUPERDB_HEALTH_ADDR, SUPERDB_MODE,
// SUPERDB_QUERY_TIMEOUT, SUPERDB_MAX_RESULT_ROWS, SUPERDB_MAX_RESULT_BYTES,
// SUPERDB_MAX_INFLIGHT_QUERIES, SUPERDB_QUERY_QUEUE_TIMEOUT,
// SUPERDB_MAX_BATCH_STATEMENTS, SUPERDB_MAX_TX_STATEMENTS,
// SUPERDB_MAX_TX_DATABASE_ROWS.
func LoadConfig(flag Config) Config {
	cfg := defaults()
	merge := func() {
		if flag.Host != "" {
			cfg.Host = flag.Host
		}
		if flag.Port != 0 {
			cfg.Port = flag.Port
		}
		if flag.DataDir != "" {
			cfg.DataDir = flag.DataDir
		}
		if flag.Mode != "" {
			cfg.Mode = flag.Mode
		}
		if flag.Username != "" {
			cfg.Username = flag.Username
		}
		if flag.Password != "" {
			cfg.Password = flag.Password
		}
		if flag.TLSCertFile != "" {
			cfg.TLSCertFile = flag.TLSCertFile
		}
		if flag.TLSKeyFile != "" {
			cfg.TLSKeyFile = flag.TLSKeyFile
		}
		if flag.MaxConnections != 0 {
			cfg.MaxConnections = flag.MaxConnections
		}
		if flag.ReadTimeout != 0 {
			cfg.ReadTimeout = flag.ReadTimeout
		}
		if flag.WriteTimeout != 0 {
			cfg.WriteTimeout = flag.WriteTimeout
		}
		if flag.IdleTimeout != 0 {
			cfg.IdleTimeout = flag.IdleTimeout
		}
		if flag.ShutdownTimeout != 0 {
			cfg.ShutdownTimeout = flag.ShutdownTimeout
		}
		if flag.HealthAddr != "" {
			cfg.HealthAddr = flag.HealthAddr
		}
		if flag.ClusterAddr != "" {
			cfg.ClusterAddr = flag.ClusterAddr
		}
		if flag.AdvertiseAddr != "" {
			cfg.AdvertiseAddr = flag.AdvertiseAddr
		}
		if len(flag.JoinAddrs) > 0 {
			cfg.JoinAddrs = flag.JoinAddrs
		}
		if flag.Region != "" {
			cfg.Region = flag.Region
		}
		mergeLimits(&cfg.Limits, flag.Limits)
	}
	// Env first, then explicit flags override.
	if v := os.Getenv("SUPERDB_HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("SUPERDB_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
		}
	} else if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
		}
	}
	if v := os.Getenv("SUPERDB_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("SUPERDB_MODE"); v != "" {
		cfg.Mode = v
	}
	if v := os.Getenv("SUPERDB_USERNAME"); v != "" {
		cfg.Username = v
	}
	if v := os.Getenv("SUPERDB_PASSWORD"); v != "" {
		cfg.Password = v
	}
	if v := os.Getenv("SUPERDB_TLS_CERT"); v != "" {
		cfg.TLSCertFile = v
	}
	if v := os.Getenv("SUPERDB_TLS_KEY"); v != "" {
		cfg.TLSKeyFile = v
	}
	if v := os.Getenv("SUPERDB_MAX_CONNECTIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxConnections = n
		}
	}
	if v := os.Getenv("SUPERDB_HEALTH_ADDR"); v != "" {
		cfg.HealthAddr = v
	}
	if v := os.Getenv("SUPERDB_QUERY_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Limits.QueryTimeout = d
		}
	}
	if v := os.Getenv("SUPERDB_QUERY_QUEUE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Limits.AcquireTimeout = d
		}
	}
	for env, dst := range map[string]*int{
		"SUPERDB_MAX_RESULT_ROWS":      &cfg.Limits.MaxResultRows,
		"SUPERDB_MAX_RESULT_BYTES":     &cfg.Limits.MaxResultBytes,
		"SUPERDB_MAX_INFLIGHT_QUERIES": &cfg.Limits.MaxInflightQueries,
		"SUPERDB_MAX_BATCH_STATEMENTS": &cfg.Limits.MaxBatchStatements,
		"SUPERDB_MAX_TX_STATEMENTS":    &cfg.Limits.MaxTxStatements,
		"SUPERDB_MAX_TX_DATABASE_ROWS": &cfg.Limits.MaxTxDatabaseRows,
	} {
		if v := os.Getenv(env); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	merge()
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 128
	}
	return cfg
}

// mergeLimits copies each explicitly-set (non-zero) limit from src.
func mergeLimits(dst *server.Limits, src server.Limits) {
	if src.QueryTimeout != 0 {
		dst.QueryTimeout = src.QueryTimeout
	}
	if src.MaxResultRows != 0 {
		dst.MaxResultRows = src.MaxResultRows
	}
	if src.MaxResultBytes != 0 {
		dst.MaxResultBytes = src.MaxResultBytes
	}
	if src.MaxInflightQueries != 0 {
		dst.MaxInflightQueries = src.MaxInflightQueries
	}
	if src.AcquireTimeout != 0 {
		dst.AcquireTimeout = src.AcquireTimeout
	}
	if src.MaxConnections != 0 {
		dst.MaxConnections = src.MaxConnections
	}
	if src.MaxBatchStatements != 0 {
		dst.MaxBatchStatements = src.MaxBatchStatements
	}
	if src.MaxTxStatements != 0 {
		dst.MaxTxStatements = src.MaxTxStatements
	}
	if src.MaxTxDatabaseRows != 0 {
		dst.MaxTxDatabaseRows = src.MaxTxDatabaseRows
	}
	if src.MaxRequestBytes != 0 {
		dst.MaxRequestBytes = src.MaxRequestBytes
	}
}

// Authenticator verifies credentials with constant-time comparison.
// Passwords are hashed with SHA-256 before comparison so the raw secret
// never stays in a comparable buffer longer than needed, and nothing
// secret is ever logged.
type Authenticator struct {
	usernameHash [32]byte
	passwordHash [32]byte
	required     bool
}

func NewAuthenticator(username, password string) Authenticator {
	if username == "" && password == "" {
		return Authenticator{}
	}
	return Authenticator{
		usernameHash: sha256.Sum256([]byte(username)),
		passwordHash: sha256.Sum256([]byte(password)),
		required:     true,
	}
}

// Required reports whether clients must authenticate.
func (a Authenticator) Required() bool { return a.required }

// Verify checks a username/password pair in constant time.
func (a Authenticator) Verify(username, password string) bool {
	if !a.required {
		return true
	}
	uh := sha256.Sum256([]byte(username))
	ph := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(uh[:], a.usernameHash[:]) == 1 &&
		subtle.ConstantTimeCompare(ph[:], a.passwordHash[:]) == 1
}
