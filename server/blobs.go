package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

func BlobUploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	
	var userID string
	err := DB.QueryRow("SELECT user_id FROM devices WHERE token_hash = ?", sha256Sum(token)).Scan(&userID)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 100<<20)

	blobID := uuid.New().String()
	f, err := os.Create(filepath.Join(Config.DataDir, "blobs", blobID))
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	_, err = io.Copy(f, r.Body)
	if err != nil {
		os.Remove(filepath.Join(Config.DataDir, "blobs", blobID))
		http.Error(w, "Upload failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"blob_id": blobID})
}

func BlobDownloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	
	var userID string
	err := DB.QueryRow("SELECT user_id FROM devices WHERE token_hash = ?", sha256Sum(token)).Scan(&userID)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	blobID := strings.TrimPrefix(r.URL.Path, "/api/blobs/")
	if blobID == "" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	var count int
	err = DB.QueryRow("SELECT COUNT(*) FROM items WHERE blob_path = ? AND user_id = ?", blobID, userID).Scan(&count)
	if err != nil || count == 0 {
		http.Error(w, "Forbidden or not found", http.StatusForbidden)
		return
	}

	path := filepath.Join(Config.DataDir, "blobs", blobID)
	w.Header().Set("Content-Disposition", `attachment; filename="`+blobID+`"`)
	http.ServeFile(w, r, path)
}
