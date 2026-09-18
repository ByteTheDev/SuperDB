package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

const (
	identityFileName = "cluster.json"
	identityVersion  = 1
)

// Identity is the persistent identity of one cluster node.
type Identity struct {
	NodeID    string `json:"node_id"`
	ClusterID string `json:"cluster_id"`
	Advertise string `json:"advertise_addr"`
	Version   int    `json:"version"`
}

func identityPath(dataDir string) string { return filepath.Join(dataDir, identityFileName) }

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// LoadIdentity reads persistent identity. ok=false when no file exists.
func LoadIdentity(dataDir string) (id Identity, ok bool, err error) {
	b, rerr := os.ReadFile(identityPath(dataDir))
	if os.IsNotExist(rerr) {
		return Identity{}, false, nil
	}
	if rerr != nil {
		return Identity{}, false, NewError(CodePersist, "read cluster identity: "+rerr.Error())
	}
	var parsed Identity
	if jerr := json.Unmarshal(b, &parsed); jerr != nil {
		return Identity{}, false, NewError(CodeCorruptMetadata, "invalid cluster.json: "+jerr.Error())
	}
	if parsed.Version != identityVersion || parsed.NodeID == "" || parsed.ClusterID == "" {
		return Identity{}, false, NewError(CodeCorruptMetadata, "cluster.json missing node_id/cluster_id/version")
	}
	return parsed, true, nil
}

// StoreIdentity atomically persists identity.
func StoreIdentity(dataDir string, id Identity) error {
	id.Version = identityVersion
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return NewError(CodePersist, "encode cluster identity: "+err.Error())
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return NewError(CodePersist, "create data dir: "+err.Error())
	}
	tmp := identityPath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return NewError(CodePersist, "write cluster identity: "+err.Error())
	}
	if err := os.Rename(tmp, identityPath(dataDir)); err != nil {
		_ = os.Remove(tmp)
		return NewError(CodePersist, "persist cluster identity: "+err.Error())
	}
	return nil
}
