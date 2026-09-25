package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"bytes"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// SendMessageRequest/Response and DownloadMediaRequest/Response are the REST
// API's wire types, shared between the bridge's HTTP layer and its callers.
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// extractTextContent pulls plain text out of a WhatsApp message, if any.
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}
	return ""
}

// sendWhatsAppMessage sends a text or media message through an already-connected client.
func sendWhatsAppMessage(client *whatsmeow.Client, recipient string, message string, mediaPath string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	var recipientJID types.JID
	var err error

	isJID := strings.Contains(recipient, "@")
	if isJID {
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
	} else {
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net",
		}
	}

	msg := &waProto.Message{}

	if mediaPath != "" {
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err)
		}

		fileExt := strings.ToLower(mediaPath[strings.LastIndex(mediaPath, ".")+1:])
		var mediaType whatsmeow.MediaType
		var mimeType string

		switch fileExt {
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/octet-stream"
		}

		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption: proto.String(message), Mimetype: proto.String(mimeType),
				URL: &resp.URL, DirectPath: &resp.DirectPath, MediaKey: resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256, FileSHA256: resp.FileSHA256, FileLength: &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			var seconds uint32 = 30
			var waveform []byte
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			}
			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype: proto.String(mimeType), URL: &resp.URL, DirectPath: &resp.DirectPath,
				MediaKey: resp.MediaKey, FileEncSHA256: resp.FileEncSHA256, FileSHA256: resp.FileSHA256,
				FileLength: &resp.FileLength, Seconds: proto.Uint32(seconds), PTT: proto.Bool(true), Waveform: waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption: proto.String(message), Mimetype: proto.String(mimeType),
				URL: &resp.URL, DirectPath: &resp.DirectPath, MediaKey: resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256, FileSHA256: resp.FileSHA256, FileLength: &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waProto.DocumentMessage{
				Title:   proto.String(mediaPath[strings.LastIndex(mediaPath, "/")+1:]),
				Caption: proto.String(message), Mimetype: proto.String(mimeType),
				URL: &resp.URL, DirectPath: &resp.DirectPath, MediaKey: resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256, FileSHA256: resp.FileSHA256, FileLength: &resp.FileLength,
			}
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	_, err = client.SendMessage(context.Background(), recipientJID, msg)
	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}
	return true, fmt.Sprintf("Message sent to %s", recipient)
}

// extractMediaInfo pulls the media metadata whatsmeow needs to later download the file.
// directPath comes straight from the message's own DirectPath field - it must NOT be
// re-derived by parsing the URL later, since WhatsApp's CDN rejects a reconstructed
// path that doesn't exactly match the one the server issued (403 Forbidden).
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, directPath string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", "", nil, nil, nil, 0
	}
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetDirectPath(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetDirectPath(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetDirectPath(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			doc.GetURL(), doc.GetDirectPath(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}
	return "", "", "", "", nil, nil, nil, 0
}

// handleMessage persists an incoming/outgoing message event for the owning session.
func handleMessage(sessionID string, client *whatsmeow.Client, mongoStore *MongoStore, msg *events.Message, logger waLog.Logger) {
	ctx := context.Background()
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	name := GetChatName(ctx, sessionID, client, mongoStore, msg.Info.Chat, chatJID, nil, sender, logger)

	if err := mongoStore.StoreChat(ctx, sessionID, chatJID, name, msg.Info.Timestamp); err != nil {
		logger.Warnf("[%s] Failed to store chat: %v", sessionID, err)
	}

	content := extractTextContent(msg.Message)
	mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	if content == "" && mediaType == "" {
		return
	}

	err := mongoStore.StoreMessage(ctx, sessionID, msg.Info.ID, chatJID, sender, content, msg.Info.Timestamp, msg.Info.IsFromMe,
		mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength)
	if err != nil {
		logger.Warnf("[%s] Failed to store message: %v", sessionID, err)
		return
	}

	timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
	direction := "←"
	if msg.Info.IsFromMe {
		direction = "→"
	}
	if mediaType != "" {
		fmt.Printf("[%s][%s] %s %s: [%s: %s] %s\n", sessionID, timestamp, direction, sender, mediaType, filename, content)
	} else if content != "" {
		fmt.Printf("[%s][%s] %s %s: %s\n", sessionID, timestamp, direction, sender, content)
	}
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface from stored metadata.
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

func (d *MediaDownloader) GetDirectPath() string             { return d.DirectPath }
func (d *MediaDownloader) GetURL() string                    { return d.URL }
func (d *MediaDownloader) GetMediaKey() []byte               { return d.MediaKey }
func (d *MediaDownloader) GetFileLength() uint64             { return d.FileLength }
func (d *MediaDownloader) GetFileSHA256() []byte             { return d.FileSHA256 }
func (d *MediaDownloader) GetFileEncSHA256() []byte          { return d.FileEncSHA256 }
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType { return d.MediaType }

// downloadMedia fetches (or returns an already-downloaded) media file for a session's message.
// Files are namespaced under store/<sessionID>/... so tenants sharing the same volume can't collide.
func downloadMedia(ctx context.Context, sessionID string, client *whatsmeow.Client, mongoStore *MongoStore, messageID, chatJID string) (bool, string, string, string, error) {
	mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength, err := mongoStore.GetMediaInfo(ctx, sessionID, messageID, chatJID)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
	}
	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	chatDir := fmt.Sprintf("store/%s/%s", sessionID, strings.ReplaceAll(chatJID, ":", "_"))
	if err := os.MkdirAll(chatDir, 0755); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	localPath := fmt.Sprintf("%s/%s", chatDir, filename)
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	if _, err := os.Stat(localPath); err == nil {
		return true, mediaType, filename, absPath, nil
	}

	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}

	if directPath == "" {
		// Legacy data stored before this field was captured - best-effort fallback,
		// though WhatsApp's CDN may reject a reconstructed path with a 403.
		directPath = extractDirectPathFromURL(url)
	}

	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL: url, DirectPath: directPath, MediaKey: mediaKey, FileLength: fileLength,
		FileSHA256: fileSHA256, FileEncSHA256: fileEncSHA256, MediaType: waMediaType,
	}

	mediaData, err := client.Download(ctx, downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}
	if err := os.WriteFile(localPath, mediaData, 0644); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	return true, mediaType, filename, absPath, nil
}

// extractDirectPathFromURL extracts the WhatsApp media server's direct path from a media URL.
func extractDirectPathFromURL(url string) string {
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url
	}
	pathPart := parts[1]
	pathPart = strings.SplitN(pathPart, "?", 2)[0]
	return "/" + pathPart
}

// GetChatName determines the display name for a chat, preferring a name already stored for this session.
func GetChatName(ctx context.Context, sessionID string, client *whatsmeow.Client, mongoStore *MongoStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	if existingName, err := mongoStore.GetChatName(ctx, sessionID, chatJID); err == nil && existingName != "" {
		return existingName
	}

	var name string

	if jid.Server == "g.us" {
		if conversation != nil {
			var displayName, convName *string
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}
		if name == "" {
			groupInfo, err := client.GetGroupInfo(ctx, jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}
	} else {
		contact, err := client.Store.Contacts.GetContact(ctx, jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			name = sender
		} else {
			name = jid.User
		}
	}

	return name
}

// handleHistorySync persists the chat/message backfill WhatsApp sends after pairing.
func handleHistorySync(sessionID string, client *whatsmeow.Client, mongoStore *MongoStore, historySync *events.HistorySync, logger waLog.Logger) {
	ctx := context.Background()
	fmt.Printf("[%s] Received history sync event with %d conversations\n", sessionID, len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		if conversation.ID == nil {
			continue
		}
		chatJID := *conversation.ID

		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("[%s] Failed to parse JID %s: %v", sessionID, chatJID, err)
			continue
		}

		name := GetChatName(ctx, sessionID, client, mongoStore, jid, chatJID, conversation, "", logger)

		messages := conversation.Messages
		if len(messages) == 0 {
			continue
		}

		latestMsg := messages[0]
		if latestMsg == nil || latestMsg.Message == nil {
			continue
		}

		timestamp := time.Time{}
		if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
			timestamp = time.Unix(int64(ts), 0)
		} else {
			continue
		}

		if err := mongoStore.StoreChat(ctx, sessionID, chatJID, name, timestamp); err != nil {
			logger.Warnf("[%s] Failed to store chat: %v", sessionID, err)
		}

		for _, msg := range messages {
			if msg == nil || msg.Message == nil {
				continue
			}

			var content string
			if msg.Message.Message != nil {
				if conv := msg.Message.Message.GetConversation(); conv != "" {
					content = conv
				} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
					content = ext.GetText()
				}
			}

			var mediaType, filename, url, directPath string
			var mediaKey, fileSHA256, fileEncSHA256 []byte
			var fileLength uint64
			if msg.Message.Message != nil {
				mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
			}

			if content == "" && mediaType == "" {
				continue
			}

			var sender string
			isFromMe := false
			if msg.Message.Key != nil {
				if msg.Message.Key.FromMe != nil {
					isFromMe = *msg.Message.Key.FromMe
				}
				if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
					sender = *msg.Message.Key.Participant
				} else if isFromMe {
					sender = client.Store.ID.User
				} else {
					sender = jid.User
				}
			} else {
				sender = jid.User
			}

			msgID := ""
			if msg.Message.Key != nil && msg.Message.Key.ID != nil {
				msgID = *msg.Message.Key.ID
			}

			msgTimestamp := time.Time{}
			if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
				msgTimestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			err = mongoStore.StoreMessage(ctx, sessionID, msgID, chatJID, sender, content, msgTimestamp, isFromMe,
				mediaType, filename, url, directPath, mediaKey, fileSHA256, fileEncSHA256, fileLength)
			if err != nil {
				logger.Warnf("[%s] Failed to store history message: %v", sessionID, err)
			} else {
				syncedCount++
			}
		}
	}

	fmt.Printf("[%s] History sync complete. Stored %d messages.\n", sessionID, syncedCount)
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file.
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	var lastGranule uint64
	var sampleRate uint32 = 48000
	var preSkip uint16
	var foundOpusHead bool

	for i := 0; i < len(data); {
		if i+27 >= len(data) {
			break
		}
		if string(data[i:i+4]) != "OggS" {
			i++
			continue
		}

		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		if !foundOpusHead && pageSeqNum <= 1 {
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 && headPos+12 < len(pageData) {
				headPos += 8
				if headPos+12 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
				}
			}
		}

		if granulePos != 0 {
			lastGranule = granulePos
		}
		i += pageSize
	}

	if lastGranule > 0 {
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
	} else {
		durationEstimate := float64(len(data)) / 2000.0
		duration = uint32(durationEstimate)
	}

	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	waveform = placeholderWaveform(duration)
	return duration, waveform, nil
}

func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages.
func placeholderWaveform(duration uint32) []byte {
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	rand.Seed(int64(duration))

	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		pos := float64(i) / float64(waveformLength)

		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)
		val += (rand.Float64() - 0.5) * 15

		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)
		val += 50

		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
