package main

import (
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type WSClient struct {
	url      string
	deviceID string
	token    string
	name     string
	os       string

	conn     *websocket.Conn
	mu       sync.Mutex
	sendChan chan WsMsg

	onDeliver         func(WsMsg)
	onPresence        func(WsMsg)
	onApprovalRequest func(WsMsg)

	devices          []Device
	pendingApprovals []WsMsg
	stateMu          sync.RWMutex
}

func NewWSClient(serverURL, deviceID, token, name, os string) *WSClient {
	u, err := url.Parse(serverURL)
	if err == nil {
		if u.Scheme == "https" {
			u.Scheme = "wss"
		} else {
			u.Scheme = "ws"
		}
		u.Path = "/ws"
		serverURL = u.String()
	}

	return &WSClient{
		url:              serverURL,
		deviceID:         deviceID,
		token:            token,
		name:             name,
		os:               os,
		sendChan:         make(chan WsMsg, 100),
		devices:          make([]Device, 0),
		pendingApprovals: make([]WsMsg, 0),
	}
}

func (c *WSClient) Start() {
	go c.writePump()
	go c.readPump()
}

func (c *WSClient) Send(msg WsMsg) {
	c.sendChan <- msg
}

func (c *WSClient) GetDevices() []Device {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	res := make([]Device, len(c.devices))
	copy(res, c.devices)
	return res
}

func (c *WSClient) GetPendingApprovals() []WsMsg {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	res := make([]WsMsg, len(c.pendingApprovals))
	copy(res, c.pendingApprovals)
	return res
}

func (c *WSClient) connect() error {
	log.Printf("WS connecting to %s", c.url)
	conn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	// Send hello
	hello := WsMsg{
		Type:     "hello",
		DeviceID: c.deviceID,
		Token:    c.token,
		Name:     c.name,
		OS:       c.os,
	}
	return conn.WriteJSON(hello)
}

func (c *WSClient) writePump() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case msg := <-c.sendChan:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn != nil {
				log.Printf("[DEBUG] Writing to websocket...")
				writeStart := time.Now()
				conn.WriteJSON(msg)
				log.Printf("[DEBUG] Websocket WriteJSON took: %v", time.Since(writeStart))
			}
		case <-ticker.C:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn != nil {
				conn.WriteMessage(websocket.PingMessage, nil)
			}
		}
	}
}

func (c *WSClient) readPump() {
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	for {
		err := c.connect()
		if err != nil {
			log.Printf("WS connect error: %v", err)
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = 1 * time.Second

		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()

		for {
			var msg WsMsg
			err := conn.ReadJSON(&msg)
			if err != nil {
				log.Printf("WS read error: %v", err)
				break
			}

			c.dispatch(msg)
		}

		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.mu.Unlock()
	}
}

func (c *WSClient) dispatch(msg WsMsg) {
	switch msg.Type {
	case "deliver":
		if c.onDeliver != nil {
			c.onDeliver(msg)
		}
		// Send ack
		c.Send(WsMsg{
			Type:   "ack",
			ItemID: msg.ItemID,
			State:  "received",
		})
	case "presence":
		c.stateMu.Lock()
		c.devices = msg.Devices
		c.stateMu.Unlock()
		if c.onPresence != nil {
			c.onPresence(msg)
		}
	case "approval_request":
		c.stateMu.Lock()
		c.pendingApprovals = append(c.pendingApprovals, msg)
		c.stateMu.Unlock()
		if c.onApprovalRequest != nil {
			c.onApprovalRequest(msg)
		}
	case "error":
		msg := msg.Message
		log.Printf("WS error from server: %s", msg)
		if strings.Contains(msg, "Unauthorized") {
			log.Printf("")
			log.Printf(">>> THIS DEVICE WAS DISCONNECTED (revoked).")
			log.Printf(">>> Run 'agent login' to log in again with your email and password.")
			log.Printf("")
			os.Exit(3)
		}
	}
}
