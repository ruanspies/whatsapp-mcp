package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// Config holds the bridge settings, all read from environment variables.
type Config struct {
	// StoreDir holds the databases, the API token and downloaded media.
	StoreDir string
	// Port is the localhost port of the REST API.
	Port int
	// ReadOnly refuses every send request.
	ReadOnly bool
	// EnableSendMedia allows /api/send to read a local file and send it.
	EnableSendMedia bool
	// SendAllowlist, when non-empty, restricts sending to these chat JIDs.
	SendAllowlist map[string]bool
	// MaxDownloadBytes is the largest media file /api/download will fetch.
	MaxDownloadBytes int64
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func loadConfig() (Config, error) {
	cfg := Config{
		StoreDir:         "store",
		Port:             8080,
		ReadOnly:         envBool("WHATSAPP_MCP_READ_ONLY"),
		EnableSendMedia:  envBool("WHATSAPP_MCP_ENABLE_SEND_MEDIA"),
		SendAllowlist:    map[string]bool{},
		MaxDownloadBytes: 100 << 20,
	}

	if v := strings.TrimSpace(os.Getenv("WHATSAPP_MCP_STORE_DIR")); v != "" {
		cfg.StoreDir = v
	}

	if v := strings.TrimSpace(os.Getenv("WHATSAPP_MCP_PORT")); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return cfg, fmt.Errorf("invalid WHATSAPP_MCP_PORT %q", v)
		}
		cfg.Port = port
	}

	if v := strings.TrimSpace(os.Getenv("WHATSAPP_MCP_MAX_DOWNLOAD_MB")); v != "" {
		mb, err := strconv.ParseInt(v, 10, 64)
		if err != nil || mb < 1 {
			return cfg, fmt.Errorf("invalid WHATSAPP_MCP_MAX_DOWNLOAD_MB %q", v)
		}
		cfg.MaxDownloadBytes = mb << 20
	}

	for _, entry := range strings.Split(os.Getenv("WHATSAPP_MCP_SEND_ALLOWLIST"), ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		jid, err := normalizeRecipient(entry)
		if err != nil {
			return cfg, fmt.Errorf("invalid WHATSAPP_MCP_SEND_ALLOWLIST entry %q: %v", entry, err)
		}
		cfg.SendAllowlist[jid.String()] = true
	}

	return cfg, nil
}

// normalizeRecipient turns a phone number or a JID string into a chat JID.
func normalizeRecipient(recipient string) (types.JID, error) {
	recipient = strings.TrimSpace(recipient)

	if strings.Contains(recipient, "@") {
		jid, err := types.ParseJID(recipient)
		if err != nil {
			return types.EmptyJID, fmt.Errorf("invalid JID: %v", err)
		}
		if jid.User == "" {
			return types.EmptyJID, fmt.Errorf("invalid JID: missing user")
		}
		return jid.ToNonAD(), nil
	}

	// Phone number: allow the usual human formatting, keep only the digits
	digits := strings.NewReplacer("+", "", " ", "", "-", "", "(", "", ")", "").Replace(recipient)
	if len(digits) < 5 || len(digits) > 20 {
		return types.EmptyJID, fmt.Errorf("phone number must be digits with country code")
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return types.EmptyJID, fmt.Errorf("phone number must be digits with country code")
		}
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}
