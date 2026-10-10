package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func main() {
	// Everything this process writes holds private messages or session keys
	restrictNewFiles()

	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	cfg, err := loadConfig()
	if err != nil {
		logger.Errorf("Invalid configuration: %v", err)
		os.Exit(1)
	}

	// Initialize message store. This also creates and restricts the store directory.
	messageStore, err := NewMessageStore(cfg.StoreDir)
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		os.Exit(1)
	}
	defer messageStore.Close()

	// Create database connection for storing session data
	ctx := context.Background()
	dbLog := waLog.Stdout("Database", "INFO", true)
	sessionDB := filepath.Join(cfg.StoreDir, "whatsapp.db")
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+sessionDB+"?_foreign_keys=on&_busy_timeout=5000", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		logger.Errorf("Failed to get device: %v", err)
		return
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	token, err := loadOrCreateToken(cfg.StoreDir)
	if err != nil {
		logger.Errorf("%v", err)
		return
	}

	bridge := &Bridge{cfg: cfg, client: client, store: messageStore, logger: logger, token: token}

	// Signalled each time the client finishes logging in
	connected := make(chan struct{}, 1)

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			bridge.handleMessage(v)

		case *events.HistorySync:
			// Process history sync events
			bridge.handleHistorySync(v)

		case *events.Contact:
			bridge.handleContactChange(v.JID)

		case *events.PushName:
			bridge.handleContactChange(v.JID, v.JIDAlt)

		case *events.BusinessName:
			bridge.handleContactChange(v.JID)

		case *events.GroupInfo:
			if v.Name != nil {
				if err := messageStore.SetChatName(v.JID.String(), v.Name.Name); err != nil {
					logger.Warnf("Failed to name chat: %v", err)
				}
			}

		case *events.JoinedGroup:
			if err := messageStore.SetChatName(v.JID.String(), v.GroupName.Name); err != nil {
				logger.Warnf("Failed to name chat: %v", err)
			}

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")
			select {
			case connected <- struct{}{}:
			default:
			}

		case *events.LoggedOut:
			logger.Warnf("Device logged out. Stop the bridge, delete %s and start it again to scan a new QR code", sessionDB)
		}
	})

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(ctx)
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Print QR code for pairing with phone
		paired := false
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				paired = true
				break
			} else {
				logger.Errorf("Pairing did not complete (%s). Start the bridge again to get a new QR code", evt.Event)
				break
			}
		}
		if !paired {
			return
		}

		fmt.Println("\nSuccessfully connected and authenticated!")
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
	}

	// Right after pairing WhatsApp drops the connection and the client reconnects,
	// so wait for the login to complete rather than for a fixed time
	select {
	case <-connected:
	case <-time.After(60 * time.Second):
		logger.Errorf("Failed to establish stable connection")
		client.Disconnect()
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp!")

	// The session database was created after the store was restricted
	restrictFiles(cfg.StoreDir)

	// Start REST API server
	if err := bridge.startRESTServer(); err != nil {
		logger.Errorf("%v", err)
		client.Disconnect()
		return
	}

	// Fill in group names and tidy up identities in the background
	go func() {
		bridge.loadGroupNames(ctx)
		bridge.mergeHiddenIdentities(ctx)
	}()

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// Disconnect client
	client.Disconnect()
}
