package remote

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

// healthServer is a tiny HTTP server for platform health checks. It never
// serves the database protocol and never exposes secrets or data.
type healthServer struct {
	addr   string
	parent *Server
	srv    *http.Server
	ln     net.Listener
}

func newHealthServer(addr string, parent *Server) *healthServer {
	h := &healthServer{addr: addr, parent: parent}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !parent.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "not_ready"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
	})
	h.srv = &http.Server{Handler: mux, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	return h
}

func (h *healthServer) serve(ctx context.Context) {
	ln, err := net.Listen("tcp", h.addr)
	if err != nil {
		return
	}
	h.ln = ln
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.srv.Shutdown(shut)
	}()
	_ = h.srv.Serve(ln)
}
