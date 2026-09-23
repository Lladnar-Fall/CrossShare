package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

type RegisterReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type LoginReq struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	DeviceName string `json:"device_name"`
	DeviceOS   string `json:"device_os"`
}

func RegisterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req RegisterReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid body", http.StatusBadRequest)
		return
	}
	
	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), 10)
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}
	
	id := uuid.New().String()
	_, err = DB.Exec("INSERT INTO users (id, email, password_hash) VALUES (?, ?, ?)", id, req.Email, string(hashed))
	if err != nil {
		http.Error(w, "Email exists", http.StatusConflict)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"user_id": id})
}

func LoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req LoginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid body", http.StatusBadRequest)
		return
	}

	var userID, hash string
	err := DB.QueryRow("SELECT id, password_hash FROM users WHERE email = ?", req.Email).Scan(&userID, &hash)
	if err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	status := "approved"

	devID := uuid.New().String()
	token := uuid.New().String()
	tokenHash := sha256Sum(token)

	_, err = DB.Exec("INSERT INTO devices (id, user_id, name, os, token_hash, status) VALUES (?, ?, ?, ?, ?, ?)",
		devID, userID, req.DeviceName, req.DeviceOS, tokenHash, status)
	
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"device_id": devID,
		"token":     token,
		"status":    status,
	})

	// If device is pending, notify all connected devices to approve it
	if status == "pending" && GlobalHub != nil {
		approvalMsg, _ := json.Marshal(map[string]string{
			"type":      "approval_request",
			"device_id": devID,
			"name":      req.DeviceName,
			"os":        req.DeviceOS,
		})
		GlobalHub.RLock()
		if users, ok := GlobalHub.Users[userID]; ok {
			for _, client := range users {
				client.send <- approvalMsg
			}
		}
		GlobalHub.RUnlock()
	}
}

func sha256Sum(s string) string {
	h := sha256.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}
