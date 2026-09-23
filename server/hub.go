package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var GlobalHub *Hub

type Hub struct {
	sync.RWMutex
	Users map[string]map[string]*Client
}

func NewHub() *Hub {
	return &Hub{
		Users: make(map[string]map[string]*Client),
	}
}

func (h *Hub) GetConnectionCount() int {
	h.RLock()
	defer h.RUnlock()
	count := 0
	for _, devs := range h.Users {
		count += len(devs)
	}
	return count
}

type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	userID   string
	deviceID string
	send     chan []byte
}

func ServeWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade err:", err)
		return
	}

	client := &Client{
		hub:  hub,
		conn: conn,
		send: make(chan []byte, 256),
	}

	go client.writePump()
	client.readPump()
}

type Msg struct {
	Type string `json:"type"`
	DeviceID string `json:"device_id,omitempty"`
	Token    string `json:"token,omitempty"`
	Name     string `json:"name,omitempty"`
	OS       string `json:"os,omitempty"`
	ItemID  string          `json:"item_id,omitempty"`
	Kind    string          `json:"kind,omitempty"`
	Mime    string          `json:"mime,omitempty"`
	Filename string         `json:"filename,omitempty"`
	Size    int64           `json:"size,omitempty"`
	Sha256  string          `json:"sha256,omitempty"`
	TTLs    int             `json:"ttl_s,omitempty"`
	Targets json.RawMessage `json:"targets,omitempty"`
	Encoding string         `json:"encoding,omitempty"`
	Payload string          `json:"payload,omitempty"`
	BlobID  string          `json:"blob_id,omitempty"`
	State string `json:"state,omitempty"`
	OriginDevice     string `json:"origin_device,omitempty"`
	OriginDeviceName string `json:"origin_device_name,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
	Live             bool   `json:"live,omitempty"`
}

func (c *Client) readPump() {
	defer func() {
		if c.deviceID != "" {
			c.hub.Lock()
			if u, ok := c.hub.Users[c.userID]; ok {
				delete(u, c.deviceID)
				if len(u) == 0 {
					delete(c.hub.Users, c.userID)
				}
			}
			c.hub.Unlock()
			DB.Exec("UPDATE devices SET last_seen = ? WHERE id = ?", time.Now(), c.deviceID)
			c.broadcastPresence()
		}
		c.conn.Close()
	}()

	var hello Msg
	if err := c.conn.ReadJSON(&hello); err != nil {
		log.Println("read hello err:", err)
		return
	}

	if hello.Type != "hello" {
		c.conn.WriteJSON(map[string]string{"type": "error", "message": "Expected hello"})
		return
	}

	var userID, status string
	err := DB.QueryRow("SELECT user_id, status FROM devices WHERE id = ? AND token_hash = ?", hello.DeviceID, sha256Sum(hello.Token)).Scan(&userID, &status)
	if err != nil || status == "revoked" {
		c.conn.WriteJSON(map[string]string{"type": "error", "message": "Unauthorized"})
		return
	}

	c.userID = userID
	c.deviceID = hello.DeviceID

	c.hub.Lock()
	if c.hub.Users[userID] == nil {
		c.hub.Users[userID] = make(map[string]*Client)
	}
	c.hub.Users[userID][c.deviceID] = c
	c.hub.Unlock()

	c.broadcastPresence()
	c.sendPendingItems()

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		var msg Msg
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "push":
			c.handlePush(msg)
		case "ack":
			DB.Exec("UPDATE deliveries SET state = ?, delivered_at = ? WHERE item_id = ? AND device_id = ?", msg.State, time.Now(), msg.ItemID, c.deviceID)
		case "approve":
			DB.Exec("UPDATE devices SET status = 'approved' WHERE id = ? AND user_id = ?", msg.DeviceID, c.userID)
			c.broadcastPresence()
		case "revoke":
			DB.Exec("UPDATE devices SET status = 'revoked' WHERE id = ? AND user_id = ?", msg.DeviceID, c.userID)
			c.hub.RLock()
			if u, ok := c.hub.Users[c.userID]; ok {
				if t, ok := u[msg.DeviceID]; ok {
					t.conn.Close()
				}
			}
			c.hub.RUnlock()
			c.broadcastPresence()
		}
	}
}

func (c *Client) writePump() {
	for msg := range c.send {
		c.conn.WriteMessage(websocket.TextMessage, msg)
	}
}

func (c *Client) broadcastPresence() {
	rows, err := DB.Query("SELECT id, name, os, status, last_seen FROM devices WHERE user_id = ? AND status != 'revoked'", c.userID)
	if err != nil {
		return
	}
	defer rows.Close()

	type DeviceResp struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		OS       string `json:"os"`
		Online   bool   `json:"online"`
		LastSeen string `json:"last_seen"`
	}

	var devices []DeviceResp
	for rows.Next() {
		var d DeviceResp
		var ls sql.NullTime
		var status string
		rows.Scan(&d.ID, &d.Name, &d.OS, &status, &ls)
		
		c.hub.RLock()
		_, isOnline := c.hub.Users[c.userID][d.ID]
		c.hub.RUnlock()
		
		d.Online = isOnline
		if ls.Valid {
			d.LastSeen = ls.Time.Format(time.RFC3339)
		}
		devices = append(devices, d)
	}

	b, _ := json.Marshal(map[string]interface{}{
		"type":    "presence",
		"devices": devices,
	})

	c.hub.RLock()
	for _, client := range c.hub.Users[c.userID] {
		client.send <- b
	}
	c.hub.RUnlock()
}

func (c *Client) sendPendingItems() {
	rows, err := DB.Query(`
		SELECT i.id, i.kind, i.mime, i.filename, i.size, i.sha256, i.encoding, i.payload, i.blob_path, i.created_at, d.name
		FROM items i
		JOIN deliveries dl ON i.id = dl.item_id
		JOIN devices d ON i.origin_device = d.id
		WHERE dl.device_id = ? AND dl.state = 'pending' AND i.expires_at > ?
		ORDER BY i.created_at ASC
	`, c.deviceID, time.Now())
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var msg Msg
		var ca time.Time
		var bp sql.NullString
		rows.Scan(&msg.ItemID, &msg.Kind, &msg.Mime, &msg.Filename, &msg.Size, &msg.Sha256, &msg.Encoding, &msg.Payload, &bp, &ca, &msg.OriginDeviceName)
		msg.Type = "deliver"
		msg.CreatedAt = ca.Format(time.RFC3339)
		msg.Live = false
		if bp.Valid && bp.String != "" {
			msg.BlobID = bp.String
		}

		b, _ := json.Marshal(msg)
		c.send <- b
	}
}

func (c *Client) handlePush(msg Msg) {
	itemID := uuid.New().String()
	if msg.ItemID != "" {
		itemID = msg.ItemID
	}

	// Get origin device name (fast read, cached by SQLite)
	var originName string
	DB.QueryRow("SELECT name FROM devices WHERE id = ?", c.deviceID).Scan(&originName)

	// Build deliver message
	delivMsg := msg
	delivMsg.Type = "deliver"
	delivMsg.OriginDevice = c.deviceID
	delivMsg.OriginDeviceName = originName
	delivMsg.CreatedAt = msg.CreatedAt // preserve sender's timestamp for transit measurement
	delivMsg.Live = true
	delivMsg.ItemID = itemID
	
	delivBytes, _ := json.Marshal(delivMsg)

	// SEND IMMEDIATELY — don't wait for DB
	c.hub.RLock()
	targets := make([]string, 0)
	for devID, tgtClient := range c.hub.Users[c.userID] {
		if devID != c.deviceID {
			tgtClient.send <- delivBytes
			targets = append(targets, devID)
		}
	}
	c.hub.RUnlock()

	// DB writes happen async — never blocks the live relay
	go func() {
		ttl := msg.TTLs
		if ttl <= 0 {
			ttl = 1800 // legacy clients that don't send ttl_s
		}
		if ttl < 60 {
			ttl = 60 // min: 1 minute
		}
		if ttl > 2592000 {
			ttl = 2592000 // max: 30 days
		}
		expiresAt := time.Now().Add(time.Duration(ttl) * time.Second)
		DB.Exec(`INSERT INTO items 
			(id, user_id, origin_device, kind, mime, filename, size, sha256, encoding, payload, blob_path, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			itemID, c.userID, c.deviceID, msg.Kind, msg.Mime, msg.Filename, msg.Size, msg.Sha256, msg.Encoding, msg.Payload, msg.BlobID, expiresAt)
		for _, tgt := range targets {
			DB.Exec("INSERT INTO deliveries (item_id, device_id) VALUES (?, ?)", itemID, tgt)
		}
	}()
}
