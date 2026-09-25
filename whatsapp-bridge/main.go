package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	logger := waLog.Stdout("Bridge", "INFO", true)
	logger.Infof("Starting WhatsApp bridge...")

	if err := os.MkdirAll("store", 0755); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	internalSecret := os.Getenv("INTERNAL_API_SECRET")
	if internalSecret == "" {
		logger.Errorf("INTERNAL_API_SECRET must be set (shared secret between this bridge and the MCP server)")
		return
	}
	if err := SetMediaEncryptionKey(os.Getenv("MEDIA_ENCRYPTION_KEY")); err != nil {
		logger.Errorf("%v", err)
		return
	}
	if mediaEncryptionEnabled() {
		logger.Infof("Media decryption keys will be encrypted at rest (MEDIA_ENCRYPTION_KEY set)")
	} else {
		logger.Warnf("MEDIA_ENCRYPTION_KEY not set - media decryption keys will be stored in plain form")
	}
	mongoURI := getenv("MONGODB_URI", "mongodb://localhost:27017")
	mongoDB := getenv("MONGODB_DATABASE", "whatsapp_mcp")
	port := getenv("PORT", "8080")

	ctx := context.Background()

	mongoStore, err := NewMongoStore(ctx, mongoURI, mongoDB)
	if err != nil {
		logger.Errorf("Failed to connect to MongoDB: %v", err)
		return
	}
	defer mongoStore.Close(context.Background())

	// whatsmeow's own per-device auth/session crypto material stays in SQLite;
	// sqlstore.Container natively supports holding many devices in one DB, one per tenant.
	dbLog := waLog.Stdout("Database", "INFO", true)
	container, err := sqlstore.New(ctx, "sqlite3", "file:store/whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to open device store: %v", err)
		return
	}

	sm := NewSessionManager(container, mongoStore, logger)
	if err := sm.Bootstrap(ctx); err != nil {
		logger.Errorf("Failed to bootstrap existing sessions: %v", err)
	}

	server := NewServer(sm, mongoStore, internalSecret)

	go func() {
		addr := fmt.Sprintf(":%s", port)
		logger.Infof("Starting internal REST API server on %s...", addr)
		if err := http.ListenAndServe(addr, server.Routes()); err != nil {
			logger.Errorf("REST API server error: %v", err)
		}
	}()

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)
	logger.Infof("Bridge is running. Press Ctrl+C to exit.")
	<-exitChan
	logger.Infof("Shutting down...")
}
