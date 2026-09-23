package main

import (
	"crypto/sha256"
	"encoding/hex"
)

type WsMsg struct {
	Type             string   `json:"type"`
	DeviceID         string   `json:"device_id,omitempty"`
	Token            string   `json:"token,omitempty"`
	Name             string   `json:"name,omitempty"`
	OS               string   `json:"os,omitempty"`
	Devices          []Device `json:"devices,omitempty"`
	ItemID           string   `json:"item_id,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	Mime             string   `json:"mime,omitempty"`
	Filename         string   `json:"filename,omitempty"`
	Size             int64    `json:"size,omitempty"`
	Sha256           string   `json:"sha256,omitempty"`
	TTLS             int      `json:"ttl_s,omitempty"`
	Targets          string   `json:"targets,omitempty"`
	Encoding         string   `json:"encoding,omitempty"` // "gzip" or "" (raw)
	Payload          string   `json:"payload,omitempty"`
	BlobID           string   `json:"blob_id,omitempty"`
	OriginDevice     string   `json:"origin_device,omitempty"`
	OriginDeviceName string   `json:"origin_device_name,omitempty"`
	CreatedAt        string   `json:"created_at,omitempty"` // RFC3339 string
	Live             bool     `json:"live,omitempty"`
	State            string   `json:"state,omitempty"`
	Message          string   `json:"message,omitempty"`
}

type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	OS       string `json:"os"`
	Online   bool   `json:"online"`
	LastSeen string `json:"last_seen"` // RFC3339 string or empty
}

type InboxItem struct {
	ItemID           string `json:"item_id"`
	Kind             string `json:"kind"`
	Mime             string `json:"mime"`
	Filename         string `json:"filename"`
	Size             int64  `json:"size"`
	Sha256           string `json:"sha256"`
	OriginDevice     string `json:"origin_device"`
	OriginDeviceName string `json:"origin_device_name"`
	CreatedAt        string `json:"created_at"`
	Payload          string `json:"payload,omitempty"`
	FilePath         string `json:"file_path,omitempty"`
	Live             bool   `json:"-"`
}

func computeSha256(data []byte) string {
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
