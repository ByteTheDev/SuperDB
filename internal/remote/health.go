package remote

import (
	"context"
	"encoding/json"
	"fmt"
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
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !parent.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "not_ready",
				"checks": map[string]string{"storage": "not_ready", "listener": "not_ready"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ready",
			"checks": map[string]string{"storage": "ready", "listener": "ready"},
		})
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		m := parent.Metrics()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetric := func(name, help, kind string, value any) {
			_, _ = fmt.Fprintf(w, "# HELP superdb_%s %s\n# TYPE superdb_%s %s\nsuperdb_%s %v\n", name, help, name, kind, name, value)
		}
		writeMetric("active_connections", "Currently active client connections.", "gauge", m.ActiveConns)
		writeMetric("connections_total", "Accepted client connections since process start.", "counter", m.TotalConns)
		writeMetric("connections_rejected_total", "Client connections rejected by admission control.", "counter", m.RejectedConns)
		writeMetric("queries_total", "Protocol requests processed since process start.", "counter", m.Queries)
		writeMetric("queries_failed_total", "Requests that returned or triggered an error.", "counter", m.FailedQueries)
		writeMetric("queries_rejected_total", "Requests rejected by query admission control.", "counter", m.RejectedQueries)
		writeMetric("auth_failures_total", "Authentication failures since process start.", "counter", m.AuthFailures)
		writeMetric("bytes_received_total", "Protocol bytes received since process start.", "counter", m.BytesRx)
		writeMetric("bytes_sent_total", "Protocol bytes sent since process start.", "counter", m.BytesTx)
		writeMetric("query_latency_average_seconds", "Average request latency since process start in seconds.", "gauge", m.AvgLatencyMs/1000)
		writeMetric("uptime_seconds", "Process uptime in seconds.", "gauge", m.UptimeSec)
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
