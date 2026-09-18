package cluster

import (
	"context"
	"net"
	"testing"

	"superdb/internal/engine"
)

func benchNode(b *testing.B, db *engine.Database) (*Node, string) {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if db == nil {
		db = engine.New()
	}
	n := New(Config{
		DataDir:           b.TempDir(),
		ListenAddr:        addr,
		AdvertiseAddr:     addr,
		HeartbeatInterval: 1000000000000,
		PingTimeout:       2000000000,
		SuspectAfter:      10000000000,
	}, db)
	ctx, cancel := context.WithTimeout(context.Background(), 10000000000)
	defer cancel()
	if err := n.Start(ctx); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(n.Shutdown)
	return n, addr
}

// Baseline: raw local engine with no cluster components (must not regress).
func BenchmarkLocalEngineExec(b *testing.B) {
	db := engine.New()
	if _, err := db.Exec("CREATE TABLE t (id INT PRIMARY KEY, v INT)"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Exec("SELECT * FROM t"); err != nil {
			b.Fatal(err)
		}
	}
}

// Same query through a single cluster node that owns the range locally:
// must stay close to the raw-engine baseline (no network on local path).
func BenchmarkClusterLocalExec(b *testing.B) {
	n, _ := benchNode(b, nil)
	ctx := context.Background()
	if _, err := n.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY, v INT)"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := n.Exec(ctx, "SELECT * FROM t"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkClusterRouting(b *testing.B) {
	n, _ := benchNode(b, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := n.Router().RouteTable("users"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkClusterMetadataLookup(b *testing.B) {
	n, _ := benchNode(b, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = n.Members().List()
		_ = n.Ranges().All()
	}
}

func BenchmarkClusterPing(b *testing.B) {
	n, addr := benchNode(b, nil)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := n.Ping(ctx, addr); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkClusterLocalVsRemote(b *testing.B) {
	n1, a1 := benchNode(b, nil)
	n2, _ := benchNode(b, nil)
	ctx := context.Background()
	if _, err := n1.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY, v INT)"); err != nil {
		b.Fatal(err)
	}
	if _, err := n1.Exec(ctx, "INSERT INTO t VALUES (1, 1)"); err != nil {
		b.Fatal(err)
	}
	b.Run("local", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := n1.Exec(ctx, "SELECT * FROM t"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("remote", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := n2.Forward(ctx, a1, "SELECT * FROM t"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkClusterConcurrentExec(b *testing.B) {
	n, _ := benchNode(b, nil)
	ctx := context.Background()
	if _, err := n.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY, v INT)"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := n.Exec(ctx, "SELECT * FROM t"); err != nil {
				b.Fatal(err)
			}
		}
	})
}
