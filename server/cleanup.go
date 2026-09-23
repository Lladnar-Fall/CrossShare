package main

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

func StartCleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		cleanupExpired()
	}
}

func cleanupExpired() {
	now := time.Now()
	rows, err := DB.Query("SELECT id, blob_path FROM items WHERE expires_at <= ?", now)
	if err != nil {
		log.Println("cleanup query err:", err)
		return
	}
	defer rows.Close()

	var toDelete []string
	for rows.Next() {
		var id, bp string
		if err := rows.Scan(&id, &bp); err != nil {
			continue
		}
		toDelete = append(toDelete, id)
		
		if bp != "" {
			os.Remove(filepath.Join(Config.DataDir, "blobs", bp))
		}
	}

	for _, id := range toDelete {
		DB.Exec("DELETE FROM deliveries WHERE item_id = ?", id)
		DB.Exec("DELETE FROM items WHERE id = ?", id)
	}
	if len(toDelete) > 0 {
		log.Printf("Cleaned up %d expired items", len(toDelete))
	}
}
