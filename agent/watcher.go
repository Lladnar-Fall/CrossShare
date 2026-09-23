package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
)

// Dedup: editors save files in ways that fire Create+Write (and sometimes
// several Writes). Same content within a few seconds = one send.
var recentFiles = struct {
	sync.Mutex
	m map[string]time.Time
}{m: make(map[string]time.Time)}

// sentState remembers path->hash of everything already shared from a
// keep-in-place folder. Persisted so restarts don't resend the world.
type sentState struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

func loadSentState(configPath string) *sentState {
	s := &sentState{m: make(map[string]string)}
	s.path = filepath.Join(filepath.Dir(configPath), "sent.json")
	if b, err := os.ReadFile(s.path); err == nil {
		json.Unmarshal(b, &s.m)
	}
	return s
}

func (s *sentState) isSame(path, hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[path] == hash
}

func (s *sentState) set(path, hash string) {
	s.mu.Lock()
	s.m[path] = hash
	b, _ := json.Marshal(s.m)
	s.mu.Unlock()
	os.WriteFile(s.path, b, 0600)
}

var keepState *sentState

func startFolderWatcher(cfg *Config, ws *WSClient, configPath string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Error creating watcher: %v", err)
		return
	}
	defer watcher.Close()

	os.MkdirAll(cfg.SendDir, 0755)
	if !cfg.SendKeep {
		os.MkdirAll(filepath.Join(cfg.SendDir, ".sent"), 0755)
	}
	os.MkdirAll(cfg.ReceivedDir, 0755)

	if cfg.SendKeep {
		keepState = loadSentState(configPath)
		if err := watchTree(watcher, cfg.SendDir); err != nil {
			log.Printf("Error watching tree: %v", err)
			return
		}
		// Initial sweep: share everything already in the folder.
		go scanKeepDir(cfg, ws)
	} else if err := watcher.Add(cfg.SendDir); err != nil {
		log.Printf("Error adding send dir to watcher: %v", err)
		return
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) {
				// Avoid processing .sent
				if filepath.Base(event.Name) == ".sent" || filepath.Base(filepath.Dir(event.Name)) == ".sent" {
					continue
				}
				// Keep mode watches recursively: pick up new subfolders.
				if cfg.SendKeep && event.Has(fsnotify.Create) {
					if fi, err := os.Stat(event.Name); err == nil && fi.IsDir() {
						watchTree(watcher, event.Name)
					}
				}
				go handleNewFile(event.Name, cfg, ws)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Watcher error: %v", err)
		}
	}
}

func handleNewFile(path string, cfg *Config, ws *WSClient) {
	time.Sleep(50 * time.Millisecond) // fast wait for write to finish

	base := filepath.Base(path)
	if strings.HasPrefix(base, ".") || base == "desktop.ini" || base == "Thumbs.db" {
		return // OS junk, never share
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}

	// Folders (in move mode AND keep mode): zip and share as one file.
	// A dropped folder may still be filling up, so wait for it to settle.
	if fi.IsDir() {
		if filepath.Base(filepath.Dir(path)) == ".sent" {
			return
		}
		time.Sleep(2 * time.Second)
		zb, err := zipDir(path)
		if err != nil {
			log.Printf("Could not share folder %s: %v", filepath.Base(path), err)
			return
		}
		if cfg.SendKeep && keepState != nil && keepState.isSame(path, computeSha256(zb)) {
			return
		}
		pushFileMsg(ws, cfg, zb, filepath.Base(path)+".zip", "application/zip", "file")
		log.Printf("Sent folder: %s (%s)", filepath.Base(path), humanBytes(int64(len(zb))))
		if cfg.SendKeep && keepState != nil {
			keepState.set(path, computeSha256(zb))
		} else if !cfg.SendKeep {
			os.Rename(path, filepath.Join(cfg.SendDir, ".sent", filepath.Base(path)))
		}
		return
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return
	}

	hash := computeSha256(b)
	size := int64(len(b))
	filename := filepath.Base(path)

	if cfg.SendKeep {
		// Keep-in-place mode: skip anything already shared with identical
		// content (covers restarts + repeat save events). New or modified
		// files always go out.
		if keepState != nil && keepState.isSame(path, hash) {
			return
		}
	} else {
		recentFiles.Lock()
		if t, ok := recentFiles.m[hash]; ok && time.Since(t) < 10*time.Second {
			recentFiles.Unlock()
			return // duplicate event for the same content
		}
		recentFiles.m[hash] = time.Now()
		for h, t := range recentFiles.m {
			if time.Since(t) > time.Minute {
				delete(recentFiles.m, h)
			}
		}
		recentFiles.Unlock()
	}

	pushFileMsg(ws, cfg, b, filename, "application/octet-stream", "file")

	log.Printf("Sent file: %s (%d bytes)", filename, size)
	if cfg.SendKeep && keepState != nil {
		keepState.set(path, hash)
	} else if !cfg.SendKeep {
		os.Rename(path, filepath.Join(cfg.SendDir, ".sent", filename))
	}
}

// pushFileMsg sends bytes as a file/image push (inline <=1MB, blob above).
// Compressible payloads go gzip'd (encoding:"gzip") — receivers gunzip.
// Small or incompressible data goes raw.
func pushFileMsg(ws *WSClient, cfg *Config, data []byte, filename, mime, kind string) {
	t0 := time.Now()
	size := int64(len(data))
	hash := computeSha256(data)
	tHash := time.Since(t0)
	payload := data
	encoding := ""
	if len(data) > 4096 {
		if gz, ok := tryGzip(data); ok {
			payload = gz
			encoding = "gzip"
		}
	}
	var blobID, b64 string
	if int64(len(payload)) <= 1024*1024 {
		b64 = base64.StdEncoding.EncodeToString(payload)
		log.Printf("Send %s (%s): hash=%v inline-b64=%v", filename, humanBytes(size), tHash, time.Since(t0)-tHash)
	} else {
		t1 := time.Now()
		blobID = uploadBlob(cfg, payload, filename)
		if blobID == "" {
			log.Printf("Failed to upload blob for %s", filename)
			return
		}
		log.Printf("Send %s (%s): hash=%v blob-upload=%v", filename, humanBytes(size), tHash, time.Since(t1))
	}
	pushFileMsgRaw(ws, cfg.TTL(), hash, size, filename, mime, kind, b64, blobID, encoding)
}

// tryGzip returns compressed bytes when they save at least 10%.
func tryGzip(data []byte) ([]byte, bool) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, false
	}
	if err := w.Close(); err != nil {
		return nil, false
	}
	if buf.Len() >= len(data)*9/10 {
		return nil, false
	}
	return buf.Bytes(), true
}

func gunzipIfNeeded(data []byte, encoding string) ([]byte, error) {
	if encoding != "gzip" {
		return data, nil
	}
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func pushFileMsgRaw(ws *WSClient, ttl int, hash string, size int64, filename, mime, kind, payload, blobID, encoding string) {
	ws.Send(WsMsg{
		Type:     "push",
		ItemID:   uuid.NewString(),
		Kind:     kind,
		Mime:     mime,
		Filename: filename,
		Size:     size,
		Sha256:   hash,
		TTLS:     ttl,
		Targets:  "all",
		Encoding: encoding,
		Payload:  payload,
		BlobID:   blobID,
	})
}

// watchTree adds a directory and all its subdirectories to the watcher.
func watchTree(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".sent" {
			return filepath.SkipDir
		}
		if err := watcher.Add(path); err != nil {
			log.Printf("Cannot watch %s: %v", path, err)
		}
		return nil
	})
}

// scanKeepDir shares everything already sitting in a keep-in-place folder
// (e.g. point it at Desktop and the whole Desktop uploads once).
func scanKeepDir(cfg *Config, ws *WSClient) {
	entries, err := os.ReadDir(cfg.SendDir)
	if err != nil {
		return
	}
	n := 0
	for _, e := range entries {
		full := filepath.Join(cfg.SendDir, e.Name())
		if e.IsDir() {
			if e.Name() == ".sent" {
				continue
			}
			handleNewFile(full, cfg, ws) // zipped as one file
			n++
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.Size() > 100<<20 {
			if err == nil {
				log.Printf("Skipping %s: over 100MB, send it manually with 'agent send-file'", e.Name())
			}
			continue
		}
		handleNewFile(full, cfg, ws)
		n++
	}
	if n > 0 {
		log.Printf("Shared %d existing file(s) from %s", n, cfg.SendDir)
	}
}

// zipDir archives a folder (for copy-pasted/dropped folders).
// Portable: lives here so every OS build can zip dropped folders.
func zipDir(src string) ([]byte, error) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	err := filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		f, err := w.Create(rel)
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(f, in)
		return err
	})
	if err != nil {
		w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > 100<<20 {
		return nil, fmt.Errorf("folder too big (>100MB)")
	}
	return buf.Bytes(), nil
}

func uploadBlob(cfg *Config, data []byte, filename string) string {
	req, err := http.NewRequest("POST", cfg.ServerURL+"/api/blobs?filename="+filename, bytes.NewReader(data))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+cfg.DeviceToken)

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()

	var res struct {
		BlobID string `json:"blob_id"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	return res.BlobID
}

func downloadBlob(cfg *Config, blobID string, path string) error {
	req, err := http.NewRequest("GET", cfg.ServerURL+"/api/blobs/"+blobID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.DeviceToken)

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func handleIncomingFile(msg WsMsg, cfg *Config) string {
	t0 := time.Now()
	outPath := filepath.Join(cfg.ReceivedDir, msg.Filename)
	if msg.BlobID != "" {
		if err := downloadBlob(cfg, msg.BlobID, outPath); err != nil {
			log.Printf("Failed to download blob %s: %v", msg.BlobID, err)
			return ""
		}
		if err := decodeFileInPlace(outPath, msg.Encoding); err != nil {
			log.Printf("Failed to decode %s: %v", outPath, err)
			return ""
		}
		log.Printf("Received file: %s (%s) in %v", outPath, humanBytes(msg.Size), time.Since(t0))
		return outPath
	}
	if msg.Payload != "" {
		data, err := base64.StdEncoding.DecodeString(msg.Payload)
		if err != nil {
			log.Printf("Failed to decode inline file payload: %v", err)
			return ""
		}
		if data, err = gunzipIfNeeded(data, msg.Encoding); err != nil {
			log.Printf("Failed to decode %s: %v", msg.Filename, err)
			return ""
		}
		os.WriteFile(outPath, data, 0644)
		log.Printf("Received file: %s (%s) in %v", outPath, humanBytes(msg.Size), time.Since(t0))
		return outPath
	}
	return ""
}

// decodeFileInPlace gunzips a downloaded blob file when needed.
func decodeFileInPlace(path, encoding string) error {
	if encoding != "gzip" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	data, err := gunzipIfNeeded(raw, encoding)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
