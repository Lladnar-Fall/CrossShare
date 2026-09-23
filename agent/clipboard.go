package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/atotto/clipboard"
	"github.com/google/uuid"
)

type ClipboardState struct {
	lastSentHash      string
	lastAppliedHash   string
	lastSentFileFP    string // fingerprint of last sent Explorer-copied files
	lastAppliedFileFP string // fingerprint of last applied (received) files
	mu                sync.Mutex
}

// fileListFingerprint identifies a clipboard file list without reading
// contents: paths + sizes + mtimes. Receiving sets the applied FP to the
// saved copy, so the watcher's next poll recognizes its own write.
func fileListFingerprint(paths []string) string {
	var sb strings.Builder
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			sb.WriteString(p + "|?;")
			continue
		}
		fmt.Fprintf(&sb, "%s|%d|%d;", p, fi.Size(), fi.ModTime().UnixNano())
	}
	return computeSha256([]byte(sb.String()))
}

func startClipboardWatcher(ws *WSClient, state *ClipboardState, cfg *Config) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		// Native copies first: Explorer files/folders, then screenshots.
		// Each returns true when the clipboard held that kind (sent or
		// echo), so text polling doesn't double-handle it.
		if checkNativeFileCopy(ws, state, cfg) {
			continue
		}
		if checkNativeImageCopy(ws, state, cfg) {
			continue
		}
		// start := time.Now()
		text, err := clipboard.ReadAll()
		// if time.Since(start) > 1*time.Second { log.Printf("Slow ReadAll: %v", time.Since(start)) }

		if err == nil && text != "" {
			hash := computeSha256([]byte(text))
			state.mu.Lock()
			if hash != state.lastSentHash && hash != state.lastAppliedHash {
				state.lastSentHash = hash
				state.mu.Unlock()

				payload := base64.StdEncoding.EncodeToString([]byte(text))
				log.Printf("[DEBUG] Text copied locally. Sending to server...")

				nowStr := time.Now().UTC().Format(time.RFC3339Nano)

				sendStart := time.Now()
				ws.Send(WsMsg{
					Type:      "push",
					ItemID:    uuid.NewString(),
					Kind:      "text",
					Mime:      "text/plain",
					Size:      int64(len(text)),
					Sha256:    hash,
					TTLS:      cfg.TTL(),
					Targets:   "all",
					Payload:   payload,
					CreatedAt: nowStr,
				})
				log.Printf("[DEBUG] Send to channel took: %v", time.Since(sendStart))
			} else {
				state.mu.Unlock()
			}
		}

		// Disable image polling temporarily to rule it out as the cause of the 5s delay
		// pollImageClipboard(ws, state)
	}
}

func applyClipboard(msg WsMsg, state *ClipboardState) {
	if !msg.Live {
		return
	}

	log.Printf("[DEBUG] Applying remote clipboard to local OS...")

	state.mu.Lock()
	if msg.Sha256 == state.lastSentHash {
		state.mu.Unlock()
		return
	}
	state.lastAppliedHash = msg.Sha256
	state.mu.Unlock()

	if msg.Kind == "text" {
		data, err := base64.StdEncoding.DecodeString(msg.Payload)
		if err == nil {
			clipboard.WriteAll(string(data))
		}
	} else if msg.Kind == "image" {
		data, err := base64.StdEncoding.DecodeString(msg.Payload)
		if err == nil {
			if data, err = gunzipIfNeeded(data, msg.Encoding); err == nil {
				// Native first (Windows: real DIB on the clipboard, pastable
				// into Paint/chat). Falls back to the old xclip helper.
				if nerr := nativeSetClipboardImagePNG(data); nerr != nil {
					writeImageClipboard(data)
				}
			}
		}
	}
}

// applyFileToClipboard puts a received file on the local clipboard as a
// file drop (CF_HDROP on Windows), so the user can Ctrl+V paste it.
// Backlog items (Live=false) only land in the folder, never the clipboard.
func applyFileToClipboard(msg WsMsg, outPath string, state *ClipboardState) {
	if !msg.Live || outPath == "" {
		return
	}
	if err := nativeSetClipboardFiles([]string{outPath}); err != nil {
		return
	}
	fp := fileListFingerprint([]string{outPath})
	state.mu.Lock()
	state.lastAppliedFileFP = fp
	state.mu.Unlock()
}

func pollImageClipboard(ws *WSClient, state *ClipboardState) {
	// Simple powershell/xclip wrapper
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// Not fully implementing the complex PS script to avoid huge block, just a skeleton for image
		// script := `Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.Clipboard]::GetImage()`
	} else if runtime.GOOS == "linux" {
		cmd = exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o")
		out, err := cmd.Output()
		if err == nil && len(out) > 0 {
			hash := computeSha256(out)
			state.mu.Lock()
			if hash != state.lastSentHash && hash != state.lastAppliedHash {
				state.lastSentHash = hash
				state.mu.Unlock()

				payload := base64.StdEncoding.EncodeToString(out)
				ws.Send(WsMsg{
					Type:    "push",
					ItemID:  uuid.NewString(),
					Kind:    "image",
					Mime:    "image/png",
					Size:    int64(len(out)),
					Sha256:  hash,
					TTLS:    1800,
					Targets: "all",
					Payload: payload,
				})
			} else {
				state.mu.Unlock()
			}
		}
	}
}

func writeImageClipboard(data []byte) {
	if runtime.GOOS == "linux" {
		cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-i")
		cmd.Stdin = bytes.NewReader(data)
		cmd.Run()
	} else if runtime.GOOS == "windows" {
		// powershell write image
	}
}
