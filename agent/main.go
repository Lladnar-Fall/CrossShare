package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	home, _ := os.UserHomeDir()

	// If we ARE the tray executable, go straight to tray mode (no CLI, no console).
	if isTrayExe() {
		runTray()
		return
	}

	defaultConfig := filepath.Join(home, ".devdrop", "config.json")

	serverURL := flag.String("server", "", "Relay server URL (e.g. https://crossshare.up.railway.app)")
	configPath := flag.String("config", defaultConfig, "Config file path")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// One-shot commands: agent status | devices | send | send-file | disconnect | logout | help
	if handled, code := runCLICommand(flag.Args(), cfg, *configPath); handled {
		os.Exit(code)
	}

	// Daemon mode from here on: log to file as well as console.
	setupFileLogging(home)

	// Override server URL from flag if provided
	if *serverURL != "" {
		cfg.ServerURL = strings.TrimRight(*serverURL, "/")
		saveConfig(cfg, *configPath)
	} else {
		cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")
	}

	// Already logged in? If this is an interactive terminal, offer the
	// same account or a switch. Headless (tray/pipe) just continues.
	if cfg.DeviceID != "" && cfg.DeviceToken != "" && isTerminal() {
		who := cfg.Email
		if who == "" {
			who = cfg.DeviceName
		}
		fmt.Printf("Logged in as %s  (server %s)\n", who, cfg.ServerURL)
		fmt.Print("Press Enter to continue, or type 'switch' for another account: ")
		if isSwitchAnswer(readLineTimeout(20 * time.Second)) {
			cfg.DeviceID = ""
			cfg.DeviceToken = ""
			cfg.Email = ""
			saveConfig(cfg, *configPath)
		}
	}

	if cfg.DeviceID == "" || cfg.DeviceToken == "" {
		doLoginFlow(cfg, *configPath)
	}

	os.MkdirAll(cfg.SendDir, 0755)
	os.MkdirAll(cfg.ReceivedDir, 0755)

	ws := NewWSClient(cfg.ServerURL, cfg.DeviceID, cfg.DeviceToken, cfg.DeviceName, cfg.DeviceOS)
	inbox := NewInbox(100)
	api := NewLocalAPI(cfg, ws, inbox)

	clipboardState := &ClipboardState{}

	ws.onDeliver = func(msg WsMsg) {
		transitTime := "unknown"
		if msg.CreatedAt != "" {
			t, err := time.Parse(time.RFC3339Nano, msg.CreatedAt)
			if err == nil {
				transitTime = time.Since(t).String()
			}
		}
		log.Printf("[DEBUG] Received message from server: Kind=%s from %s. Transit time: %s", msg.Kind, msg.OriginDeviceName, transitTime)

		item := InboxItem{
			ItemID:           msg.ItemID,
			Kind:             msg.Kind,
			Mime:             msg.Mime,
			Filename:         msg.Filename,
			Size:             msg.Size,
			Sha256:           msg.Sha256,
			OriginDevice:     msg.OriginDevice,
			OriginDeviceName: msg.OriginDeviceName,
			CreatedAt:        msg.CreatedAt,
			Live:             msg.Live,
		}

		if msg.Kind == "text" || msg.Kind == "image" {
			item.Payload = msg.Payload
			applyClipboard(msg, clipboardState)
		} else if msg.Kind == "file" {
			outPath := handleIncomingFile(msg, cfg)
			item.FilePath = outPath
			applyFileToClipboard(msg, outPath, clipboardState)
		}

		inbox.Add(item)
		api.BroadcastEvent(map[string]interface{}{
			"type": "incoming",
			"item": item,
		})
		log.Printf("Received %s from %s", msg.Kind, msg.OriginDeviceName)
	}

	ws.onPresence = func(msg WsMsg) {
		api.BroadcastEvent(map[string]interface{}{
			"type":    "presence",
			"devices": msg.Devices,
		})
	}

	ws.onApprovalRequest = func(msg WsMsg) {
		log.Printf("New device wants approval: %s (%s)", msg.Name, msg.OS)
		log.Printf("")
		log.Printf(">>> TO APPROVE THIS DEVICE, OPEN A NEW COMMAND PROMPT AND RUN THIS EXACT COMMAND: <<<")
		log.Printf("curl.exe -X POST -H \"Authorization: Bearer %s\" -H \"Content-Type: application/json\" -d \"{\\\"device_id\\\":\\\"%s\\\"}\" http://127.0.0.1:9876/api/approve", cfg.LocalAPIToken, msg.DeviceID)
		log.Printf("")
		api.BroadcastEvent(map[string]interface{}{
			"type":      "approval_request",
			"device_id": msg.DeviceID,
			"name":      msg.Name,
			"os":        msg.OS,
		})
	}

	ws.Start()
	go startClipboardWatcher(ws, clipboardState, cfg)
	go startFolderWatcher(cfg, ws, *configPath)
	go api.Start()

	log.Printf("CrossShare Agent started")
	log.Printf("  Device:    %s (%s)", cfg.DeviceName, cfg.DeviceOS)
	log.Printf("  Server:    %s", cfg.ServerURL)
	log.Printf("  Local API: 127.0.0.1:%d", cfg.LocalAPIPort)
	log.Printf("  Send dir:  %s", cfg.SendDir)
	log.Printf("  Recv dir:  %s", cfg.ReceivedDir)
	log.Printf("  Commands:  agent status | agent devices | agent send \"hi\" | agent help")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("Shutting down...")
}

func doLoginFlow(cfg *Config, configPath string) {
	fmt.Println("╔══════════════════════════════╗")
	fmt.Println("║    CrossShare Agent Setup    ║")
	fmt.Println("╚══════════════════════════════╝")
	fmt.Printf("Server: %s\n\n", cfg.ServerURL)

	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Email: ")
	email, _ := reader.ReadString('\n')
	email = strings.TrimSpace(email)

	fmt.Print("Password: ")
	password, _ := reader.ReadString('\n')
	password = strings.TrimSpace(password)

	// Try register first (ignore error if email exists)
	regPayload, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": password,
	})
	http.Post(cfg.ServerURL+"/api/register", "application/json", bytes.NewBuffer(regPayload))

	// Then login (creates device)
	loginPayload, _ := json.Marshal(map[string]string{
		"email":       email,
		"password":    password,
		"device_name": cfg.DeviceName,
		"device_os":   cfg.DeviceOS,
	})

	resp, err := http.Post(cfg.ServerURL+"/api/login", "application/json", bytes.NewBuffer(loginPayload))
	if err != nil {
		log.Fatalf("Login failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Fatalf("Login failed with status %d", resp.StatusCode)
	}

	var res struct {
		DeviceID string `json:"device_id"`
		Token    string `json:"token"`
		Status   string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&res)

	cfg.DeviceID = res.DeviceID
	cfg.DeviceToken = res.Token
	cfg.Email = email
	saveConfig(cfg, configPath)

	fmt.Printf("\n✓ Logged in! Device ID: %s\n", res.DeviceID)
	if res.Status == "pending" {
		fmt.Println("⏳ This device needs approval from an existing device.")
	} else {
		fmt.Println("✓ Device auto-approved (first device on this account).")
	}
	fmt.Println()
}
