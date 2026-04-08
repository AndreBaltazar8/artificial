package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"artificial.pt/svc-artificial/internal/db"
	"artificial.pt/svc-artificial/internal/server"
)

func main() {
	port := flag.Int("port", 4000, "HTTP port")
	dbPath := flag.String("db", "", "SQLite database path (default: ~/.config/artificial/artificial.db)")
	workerBin := flag.String("worker-bin", "", "Path to cmd-worker binary (default: auto-detect next to this binary or in PATH)")
	flag.Parse()

	if *dbPath == "" {
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".config", "artificial")
		os.MkdirAll(dir, 0755)
		*dbPath = filepath.Join(dir, "artificial.db")
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		slog.Error("open database", "err", err)
		os.Exit(1)
	}
	defer database.Close()

	// Ensure commander employee exists.
	if _, err := database.GetEmployeeByNick("commander"); err != nil {
		database.CreateEmployee("commander", "commander", "The project owner and final decision maker.", "")
	}

	srv := server.New(database, *port, *workerBin)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := srv.Run(ctx); err != nil {
		slog.Error("server", "err", err)
	}
}
