package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
)

// Server exposes the bridge's internal REST API. It is meant to be reached
// only by the Python MCP server over a private network - the caller has
// already authenticated the end user and resolved which session_id they own.
type Server struct {
	sm            *SessionManager
	mongo         *MongoStore
	internalToken string
}

func NewServer(sm *SessionManager, mongo *MongoStore, internalToken string) *Server {
	return &Server{sm: sm, mongo: mongo, internalToken: internalToken}
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	got := r.Header.Get("X-Internal-Secret")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.internalToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /internal/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /internal/sessions/{id}/status", s.handleSessionStatus)
	mux.HandleFunc("POST /internal/sessions/{id}/send", s.handleSend)
	mux.HandleFunc("POST /internal/sessions/{id}/download", s.handleDownload)

	return mux
}

// handleCreateSession starts pairing a brand-new WhatsApp device and returns
// the session id plus a QR code (base64 PNG) for the end user to scan.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}

	sessionID, qrText, err := s.sm.CreateSession(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	qrPNG, err := QRPNGBase64(qrText)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]string{
		"session_id":    sessionID,
		"qr_png_base64": qrPNG,
	})
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}

	id := r.PathValue("id")
	status, ok := s.sm.Status(id)
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "unknown session"})
		return
	}

	resp := map[string]string{"status": string(status)}
	if qrText, pending := s.sm.LatestQR(id); pending && qrText != "" {
		if qrPNG, err := QRPNGBase64(qrText); err == nil {
			resp["qr_png_base64"] = qrPNG
		}
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}

	id := r.PathValue("id")
	client, ok := s.sm.Client(id)
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	var req SendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}
	if req.Recipient == "" {
		http.Error(w, "Recipient is required", http.StatusBadRequest)
		return
	}
	if req.Message == "" && req.MediaPath == "" {
		http.Error(w, "Message or media path is required", http.StatusBadRequest)
		return
	}

	success, message := sendWhatsAppMessage(client, req.Recipient, req.Message, req.MediaPath)

	w.Header().Set("Content-Type", "application/json")
	if !success {
		w.WriteHeader(http.StatusInternalServerError)
	}
	json.NewEncoder(w).Encode(SendMessageResponse{Success: success, Message: message})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}

	id := r.PathValue("id")
	client, ok := s.sm.Client(id)
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	var req DownloadMediaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}
	if req.MessageID == "" || req.ChatJID == "" {
		http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
		return
	}

	success, mediaType, filename, path, err := downloadMedia(r.Context(), id, client, s.mongo, req.MessageID, req.ChatJID)

	w.Header().Set("Content-Type", "application/json")
	if !success || err != nil {
		errMsg := "Unknown error"
		if err != nil {
			errMsg = err.Error()
		}
		fmt.Printf("[%s] Download failed for message %s in chat %s: %s\n", id, req.MessageID, req.ChatJID, errMsg)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to download media: %s", errMsg),
		})
		return
	}

	json.NewEncoder(w).Encode(DownloadMediaResponse{
		Success:  true,
		Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
		Filename: filename,
		Path:     path,
	})
}
