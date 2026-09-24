package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"rsc.io/qr"
)

// liveSession is the in-memory, connected side of a session: the running
// whatsmeow client plus whatever pairing state is still relevant.
type liveSession struct {
	id     string
	client *whatsmeow.Client

	mu     sync.RWMutex
	status SessionStatus
	lastQR string // raw QR text of the most recently issued (possibly stale) code
}

// SessionManager owns every linked WhatsApp account (tenant) this bridge
// process is currently serving. Each tenant gets its own whatsmeow.Client,
// backed by its own device inside the shared sqlstore.Container (whatsmeow
// supports many devices per container natively) and its own slice of the
// shared MongoDB collections, scoped by session_id.
type SessionManager struct {
	container *sqlstore.Container
	mongo     *MongoStore
	logger    waLog.Logger

	mu       sync.RWMutex
	sessions map[string]*liveSession
}

func NewSessionManager(container *sqlstore.Container, mongo *MongoStore, logger waLog.Logger) *SessionManager {
	return &SessionManager{
		container: container,
		mongo:     mongo,
		logger:    logger,
		sessions:  make(map[string]*liveSession),
	}
}

// Bootstrap reconnects every already-paired session recorded in Mongo. Call once at startup.
func (sm *SessionManager) Bootstrap(ctx context.Context) error {
	docs, err := sm.mongo.ListConnectedSessions(ctx)
	if err != nil {
		return err
	}

	for _, doc := range docs {
		if doc.JID == "" {
			continue
		}
		jid, err := types.ParseJID(doc.JID)
		if err != nil {
			sm.logger.Warnf("Skipping session %s: bad JID %q: %v", doc.ID, doc.JID, err)
			continue
		}
		device, err := sm.container.GetDevice(ctx, jid)
		if err != nil || device == nil {
			sm.logger.Warnf("Skipping session %s: device not found: %v", doc.ID, err)
			continue
		}

		ls := sm.registerClient(doc.ID, device)
		if err := ls.client.Connect(); err != nil {
			sm.logger.Warnf("Failed to reconnect session %s: %v", doc.ID, err)
			continue
		}
		sm.logger.Infof("Reconnected session %s (%s)", doc.ID, doc.JID)
	}
	return nil
}

func (sm *SessionManager) registerClient(sessionID string, device *store.Device) *liveSession {
	client := whatsmeow.NewClient(device, sm.logger)
	ls := &liveSession{id: sessionID, client: client, status: StatusPending}

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			handleMessage(sessionID, client, sm.mongo, v, sm.logger)
		case *events.HistorySync:
			handleHistorySync(sessionID, client, sm.mongo, v, sm.logger)
		case *events.Connected:
			sm.logger.Infof("[%s] Connected to WhatsApp", sessionID)
			ls.mu.Lock()
			ls.status = StatusConnected
			ls.mu.Unlock()
		case *events.LoggedOut:
			sm.logger.Warnf("[%s] Device logged out, needs to re-link", sessionID)
			ls.mu.Lock()
			ls.status = StatusLoggedOut
			ls.mu.Unlock()
			if err := sm.mongo.SetSessionStatus(context.Background(), sessionID, StatusLoggedOut); err != nil {
				sm.logger.Warnf("[%s] failed to persist logged-out status: %v", sessionID, err)
			}
		}
	})

	sm.mu.Lock()
	sm.sessions[sessionID] = ls
	sm.mu.Unlock()
	return ls
}

// CreateSession starts pairing a brand-new WhatsApp device and returns the new
// session id plus the first QR code (raw text, ready to be PNG-encoded).
func (sm *SessionManager) CreateSession(ctx context.Context) (sessionID string, qrText string, err error) {
	sessionID = uuid.NewString()
	if err = sm.mongo.CreateSessionRecord(ctx, sessionID); err != nil {
		return "", "", fmt.Errorf("failed to record session: %w", err)
	}

	device := sm.container.NewDevice()
	ls := sm.registerClient(sessionID, device)

	qrChan, err := ls.client.GetQRChannel(context.Background())
	if err != nil {
		return "", "", fmt.Errorf("failed to get QR channel: %w", err)
	}
	if err = ls.client.Connect(); err != nil {
		return "", "", fmt.Errorf("failed to connect: %w", err)
	}

	first := make(chan string, 1)
	go func() {
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				ls.mu.Lock()
				ls.lastQR = evt.Code
				ls.mu.Unlock()
				select {
				case first <- evt.Code:
				default:
				}
			case "success":
				if ls.client.Store.ID == nil {
					continue
				}
				jid := ls.client.Store.ID.String()
				if err := sm.mongo.SetSessionConnected(context.Background(), sessionID, jid); err != nil {
					sm.logger.Errorf("[%s] failed to persist connected session: %v", sessionID, err)
				}
			case "timeout":
				sm.logger.Warnf("[%s] QR pairing timed out", sessionID)
				if err := sm.mongo.SetSessionStatus(context.Background(), sessionID, StatusLoggedOut); err != nil {
					sm.logger.Warnf("[%s] failed to persist timeout status: %v", sessionID, err)
				}
			}
		}
	}()

	select {
	case code := <-first:
		return sessionID, code, nil
	case <-time.After(30 * time.Second):
		return sessionID, "", fmt.Errorf("timed out waiting for QR code")
	}
}

// Status reports the live in-memory status of a session known to this process.
func (sm *SessionManager) Status(sessionID string) (SessionStatus, bool) {
	sm.mu.RLock()
	ls, ok := sm.sessions[sessionID]
	sm.mu.RUnlock()
	if !ok {
		return "", false
	}
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return ls.status, true
}

// LatestQR returns the most recently issued QR text for a still-pending session.
func (sm *SessionManager) LatestQR(sessionID string) (qrText string, stillPending bool) {
	sm.mu.RLock()
	ls, ok := sm.sessions[sessionID]
	sm.mu.RUnlock()
	if !ok {
		return "", false
	}
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return ls.lastQR, ls.status == StatusPending
}

// Client returns the live whatsmeow client for a session, if this process has it loaded.
func (sm *SessionManager) Client(sessionID string) (*whatsmeow.Client, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	ls, ok := sm.sessions[sessionID]
	if !ok {
		return nil, false
	}
	return ls.client, true
}

// QRPNGBase64 renders QR text as a base64-encoded PNG, since a hosted bridge
// can't print the code to a terminal for the user to scan.
func QRPNGBase64(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(code.PNG()), nil
}
