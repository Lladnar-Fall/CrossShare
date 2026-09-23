package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

var (
	Config struct {
		Port    string
		DataDir string
		DBPath  string
	}
)

func initConfig() {
	Config.Port = os.Getenv("PORT")
	if Config.Port == "" {
		Config.Port = "8080"
	}
	Config.DataDir = os.Getenv("DATA_DIR")
	if Config.DataDir == "" {
		Config.DataDir = "/tmp/devdrop_data"
	}
	Config.DBPath = os.Getenv("DB_PATH")
	if Config.DBPath == "" {
		// Use /tmp/ native Linux filesystem for database to prevent massive
		// 9P protocol slowdowns when running inside WSL mapped to a Windows drive.
		Config.DBPath = "/tmp/devdrop.db"
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"status":      "ok",
		"uptime":      "1000s",
		"connections": GlobalHub.GetConnectionCount(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// resolveWebDir finds the web/ client directory regardless of whether the
// server is run from the project root (go run ./server), from server/
// (go run .), or from /app in Docker. WEB_DIR env var always wins.
func resolveWebDir() string {
	if env := os.Getenv("WEB_DIR"); env != "" {
		return env
	}
	candidates := []string{
		filepath.Join(".", "web"),        // project root
		filepath.Join("..", "web"),       // server/ dir
		filepath.Join(".", "server", "web"),
		filepath.Join("/app", "web"),     // Docker WORKDIR
		filepath.Join("/app", "..", "web"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !st.IsDir() {
			return c
		}
	}
	// Fall back to ./web so the failure mode is a clear 404, not a panic.
	return filepath.Join(".", "web")
}

func main() {
	initConfig()

	err := os.MkdirAll(Config.DataDir, 0755)
	if err != nil {
		log.Fatal(err)
	}
	err = os.MkdirAll(filepath.Join(Config.DataDir, "blobs"), 0755)
	if err != nil {
		log.Fatal(err)
	}

	InitDB()
	defer DB.Close()

	GlobalHub = NewHub()

	go StartCleanup()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", RegisterHandler)
	mux.HandleFunc("/api/login", LoginHandler)
	mux.HandleFunc("/api/blobs", BlobUploadHandler)
	mux.HandleFunc("/api/blobs/", BlobDownloadHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ServeWs(GlobalHub, w, r)
	})

	// Serve the mobile PWA web UI on the root path.
	// Go's ServeMux prefers longest match, so /api/*, /ws and /health
	// still take precedence over this catch-all.
	webDir := resolveWebDir()
	mux.Handle("/", http.FileServer(http.Dir(webDir)))

	addr := fmt.Sprintf(":%s", Config.Port)
	log.Printf("Starting server on %s", addr)
	log.Fatal(http.ListenAndServe(addr, corsMiddleware(mux)))
}
