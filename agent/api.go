package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type LocalAPI struct {
	cfg   *Config
	ws    *WSClient
	inbox *Inbox

	eventConns map[*websocket.Conn]bool
	connMu     sync.Mutex
}

func NewLocalAPI(cfg *Config, ws *WSClient, inbox *Inbox) *LocalAPI {
	return &LocalAPI{
		cfg:        cfg,
		ws:         ws,
		inbox:      inbox,
		eventConns: make(map[*websocket.Conn]bool),
	}
}

func (api *LocalAPI) Start() {
	mux := http.NewServeMux()

	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			token := r.Header.Get("Authorization")
			if token != "Bearer "+api.cfg.LocalAPIToken {
				if r.URL.Query().Get("token") != api.cfg.LocalAPIToken {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next(w, r)
		}
	}

	mux.HandleFunc("/api/status", auth(func(w http.ResponseWriter, r *http.Request) {
		api.ws.mu.Lock()
		connected := api.ws.conn != nil
		api.ws.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"connected":   connected,
			"device_id":   api.cfg.DeviceID,
			"device_name": api.cfg.DeviceName,
			"server_url":  api.cfg.ServerURL,
		})
	}))

	mux.HandleFunc("/api/devices", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.ws.GetDevices())
	}))

	mux.HandleFunc("/api/inbox", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.inbox.GetAll())
	}))

	// Send text or file to other devices
	mux.HandleFunc("/api/send", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Kind     string `json:"kind"`      // "text" or "file"
			Content  string `json:"content"`   // text content
			FilePath string `json:"file_path"` // local file path
			Targets  string `json:"targets"`   // "all" or device ID
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		if req.Targets == "" {
			req.Targets = "all"
		}

		switch req.Kind {
		case "text":
			payload := base64.StdEncoding.EncodeToString([]byte(req.Content))
			hash := computeSha256([]byte(req.Content))
			api.ws.Send(WsMsg{
				Type:    "push",
				ItemID:  uuid.NewString(),
				Kind:    "text",
				Mime:    "text/plain",
				Size:    int64(len(req.Content)),
				Sha256:  hash,
				TTLS:    api.cfg.TTL(),
				Targets: req.Targets,
				Payload: payload,
			})
		case "file":
			data, err := os.ReadFile(req.FilePath)
			if err != nil {
				http.Error(w, "File not found: "+err.Error(), http.StatusBadRequest)
				return
			}
			filename := req.FilePath
			if idx := strings.LastIndexAny(filename, "/\\"); idx >= 0 {
				filename = filename[idx+1:]
			}
			pushFileMsg(api.ws, api.cfg, data, filename, "application/octet-stream", "file")
		default:
			http.Error(w, "Invalid kind", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "sent"})
	}))

	// Approve a pending device
	mux.HandleFunc("/api/approve", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			DeviceID string `json:"device_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		api.ws.Send(WsMsg{Type: "approve", DeviceID: req.DeviceID})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "approved"})
	}))

	// Revoke a device
	mux.HandleFunc("/api/revoke", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			DeviceID string `json:"device_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		api.ws.Send(WsMsg{Type: "revoke", DeviceID: req.DeviceID})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
	}))

	// Get full content of an inbox item
	mux.HandleFunc("/api/inbox/", auth(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/inbox/")
		parts := strings.Split(path, "/")
		if len(parts) == 0 {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		itemID := parts[0]

		if r.Method == "DELETE" {
			api.inbox.Remove(itemID)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
			return
		}

		// GET /api/inbox/{id}/content
		if len(parts) >= 2 && parts[1] == "content" {
			item := api.inbox.Get(itemID)
			if item == nil {
				http.Error(w, "Not found", http.StatusNotFound)
				return
			}
			if item.FilePath != "" {
				http.ServeFile(w, r, item.FilePath)
				return
			}
			if item.Payload != "" {
				data, _ := base64.StdEncoding.DecodeString(item.Payload)
				w.Header().Set("Content-Type", "text/plain")
				w.Write(data)
				return
			}
			http.Error(w, "No content", http.StatusNotFound)
			return
		}

		http.Error(w, "Not found", http.StatusNotFound)
	}))

	// WebSocket for real-time events to plugin
	mux.HandleFunc("/api/events", auth(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		api.connMu.Lock()
		api.eventConns[c] = true
		api.connMu.Unlock()

		defer func() {
			api.connMu.Lock()
			delete(api.eventConns, c)
			api.connMu.Unlock()
			c.Close()
		}()

		for {
			if _, _, err := c.ReadMessage(); err != nil {
				break
			}
		}
	}))

	addr := fmt.Sprintf("127.0.0.1:%d", api.cfg.LocalAPIPort)
	log.Printf("Local API listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("Local API error: %v", err)
	}
}

func (api *LocalAPI) BroadcastEvent(event interface{}) {
	api.connMu.Lock()
	defer api.connMu.Unlock()

	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	for c := range api.eventConns {
		if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
			c.Close()
			delete(api.eventConns, c)
		}
	}
}
