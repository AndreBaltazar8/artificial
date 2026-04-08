package server

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"net/http"

	"artificial.pt/svc-artificial/internal/db"
)

//go:embed dashboard.html
var dashboardFS embed.FS

// Server is the central HTTP + WebSocket server.
type Server struct {
	DB        *db.DB
	Hub       *Hub
	Mux       *http.ServeMux
	Port      int
	WorkerBin string // path to cmd-worker binary
}

// New creates a new server.
func New(database *db.DB, port int, workerBin string) *Server {
	s := &Server{
		DB:        database,
		Hub:       NewHub(database, port),
		Mux:       http.NewServeMux(),
		Port:      port,
		WorkerBin: workerBin,
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	// Dashboard
	s.Mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		data, err := dashboardFS.ReadFile("dashboard.html")
		if err != nil {
			http.Error(w, "dashboard not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	// WebSocket
	s.Mux.HandleFunc("GET /ws", s.Hub.HandleWebSocket)

	// REST API
	s.registerAPI()
}

// Run starts the HTTP server.
func (s *Server) Run(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.Port)
	srv := &http.Server{Addr: addr, Handler: s.Mux}

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	slog.Info("server starting", "addr", addr)
	return srv.ListenAndServe()
}
