package server

import (
	"context"
	"time"

	"superdb/internal/cluster"
	"superdb/internal/engine"
)

// ClusterConfig wires one cluster node around the server's local engine.
type ClusterConfig struct {
	DataDir       string
	ListenAddr    string
	AdvertiseAddr string
	JoinAddrs     []string
	DB            *engine.Database
}

// StartClusterNode starts cluster membership/transport/ranges for db.
// The caller owns the client SQL listener; the node only adds internal
// traffic. Local mode never calls this, so local performance is unchanged.
func StartClusterNode(cfg ClusterConfig) (*cluster.Node, error) {
	n := cluster.New(cluster.Config{
		DataDir:           cfg.DataDir,
		ListenAddr:        cfg.ListenAddr,
		AdvertiseAddr:     cfg.AdvertiseAddr,
		JoinAddrs:         cfg.JoinAddrs,
		HeartbeatInterval: 500 * time.Millisecond,
		PingTimeout:       2 * time.Second,
		SuspectAfter:      5 * time.Second,
	}, cfg.DB)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := n.Start(ctx); err != nil {
		return nil, err
	}
	return n, nil
}
