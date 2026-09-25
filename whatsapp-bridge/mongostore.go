package main

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ErrNotFound is returned when a lookup finds no matching document.
var ErrNotFound = errors.New("not found")

// SessionStatus is the lifecycle state of a linked WhatsApp session.
type SessionStatus string

const (
	StatusPending   SessionStatus = "pending"
	StatusConnected SessionStatus = "connected"
	StatusLoggedOut SessionStatus = "logged_out"
)

// MongoStore holds the app-level, multi-tenant data: the session registry,
// chats, and messages. This is distinct from whatsmeow's own sqlstore.Container,
// which keeps each device's opaque auth/session crypto material in SQLite.
type MongoStore struct {
	client   *mongo.Client
	sessions *mongo.Collection
	chats    *mongo.Collection
	messages *mongo.Collection
}

func NewMongoStore(ctx context.Context, uri, dbName string) (*MongoStore, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}

	db := client.Database(dbName)
	m := &MongoStore{
		client:   client,
		sessions: db.Collection("sessions"),
		chats:    db.Collection("chats"),
		messages: db.Collection("messages"),
	}
	if err := m.ensureIndexes(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *MongoStore) Close(ctx context.Context) error {
	return m.client.Disconnect(ctx)
}

func (m *MongoStore) ensureIndexes(ctx context.Context) error {
	if _, err := m.chats.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "session_id", Value: 1}, {Key: "jid", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		{
			// Privacy/retention: chats auto-delete this long after they were last
			// touched, unless a user calls forget_me sooner.
			Keys:    bson.D{{Key: "stored_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(dataTTLSeconds),
		},
	}); err != nil {
		return err
	}

	if _, err := m.messages.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "session_id", Value: 1}, {Key: "chat_jid", Value: 1}, {Key: "id", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		{
			Keys: bson.D{{Key: "session_id", Value: 1}, {Key: "chat_jid", Value: 1}, {Key: "timestamp", Value: -1}},
		},
		{
			Keys: bson.D{{Key: "content", Value: "text"}},
		},
		{
			// Same retention policy as chats - see above.
			Keys:    bson.D{{Key: "stored_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(dataTTLSeconds),
		},
	}); err != nil {
		return err
	}

	return nil
}

// dataTTLSeconds bounds how long chat/message documents are kept: 2 hours by
// default, so a user who never calls forget_me still isn't left with their
// WhatsApp history sitting in this database indefinitely.
const dataTTLSeconds int32 = 2 * 60 * 60

// --- Chats & messages (written by the Go bridge, read by the Python MCP server) ---

type chatDoc struct {
	SessionID       string    `bson:"session_id"`
	JID             string    `bson:"jid"`
	Name            string    `bson:"name"`
	LastMessageTime time.Time `bson:"last_message_time"`
	StoredAt        time.Time `bson:"stored_at"`
}

type messageDoc struct {
	SessionID     string    `bson:"session_id"`
	ID            string    `bson:"id"`
	ChatJID       string    `bson:"chat_jid"`
	Sender        string    `bson:"sender"`
	Content       string    `bson:"content"`
	Timestamp     time.Time `bson:"timestamp"`
	IsFromMe      bool      `bson:"is_from_me"`
	MediaType     string    `bson:"media_type,omitempty"`
	Filename      string    `bson:"filename,omitempty"`
	URL           string    `bson:"url,omitempty"`
	MediaKey      []byte    `bson:"media_key,omitempty"`
	FileSHA256    []byte    `bson:"file_sha256,omitempty"`
	FileEncSHA256 []byte    `bson:"file_enc_sha256,omitempty"`
	FileLength    uint64    `bson:"file_length,omitempty"`
	StoredAt      time.Time `bson:"stored_at"`
}

func (m *MongoStore) StoreChat(ctx context.Context, sessionID, jid, name string, lastMessageTime time.Time) error {
	_, err := m.chats.UpdateOne(ctx,
		bson.M{"session_id": sessionID, "jid": jid},
		bson.M{"$set": chatDoc{SessionID: sessionID, JID: jid, Name: name, LastMessageTime: lastMessageTime, StoredAt: time.Now()}},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

func (m *MongoStore) StoreMessage(ctx context.Context, sessionID, id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	if content == "" && mediaType == "" {
		return nil
	}

	encMediaKey, err := encryptBytes(mediaKey)
	if err != nil {
		return err
	}
	encFileSHA256, err := encryptBytes(fileSHA256)
	if err != nil {
		return err
	}
	encFileEncSHA256, err := encryptBytes(fileEncSHA256)
	if err != nil {
		return err
	}

	doc := messageDoc{
		SessionID: sessionID, ID: id, ChatJID: chatJID, Sender: sender, Content: content,
		Timestamp: timestamp, IsFromMe: isFromMe, MediaType: mediaType, Filename: filename,
		URL: url, MediaKey: encMediaKey, FileSHA256: encFileSHA256, FileEncSHA256: encFileEncSHA256,
		FileLength: fileLength, StoredAt: time.Now(),
	}
	_, err = m.messages.UpdateOne(ctx,
		bson.M{"session_id": sessionID, "chat_jid": chatJID, "id": id},
		bson.M{"$set": doc},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

// GetChatName returns the previously stored name for a chat, or "" if none is known yet.
func (m *MongoStore) GetChatName(ctx context.Context, sessionID, chatJID string) (string, error) {
	var doc chatDoc
	err := m.chats.FindOne(ctx, bson.M{"session_id": sessionID, "jid": chatJID}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return "", nil
		}
		return "", err
	}
	return doc.Name, nil
}

func (m *MongoStore) StoreMediaInfo(ctx context.Context, sessionID, id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	encMediaKey, err := encryptBytes(mediaKey)
	if err != nil {
		return err
	}
	encFileSHA256, err := encryptBytes(fileSHA256)
	if err != nil {
		return err
	}
	encFileEncSHA256, err := encryptBytes(fileEncSHA256)
	if err != nil {
		return err
	}

	_, err = m.messages.UpdateOne(ctx,
		bson.M{"session_id": sessionID, "chat_jid": chatJID, "id": id},
		bson.M{"$set": bson.M{
			"url": url, "media_key": encMediaKey, "file_sha256": encFileSHA256,
			"file_enc_sha256": encFileEncSHA256, "file_length": fileLength,
		}},
	)
	return err
}

func (m *MongoStore) GetMediaInfo(ctx context.Context, sessionID, id, chatJID string) (mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64, err error) {
	var doc messageDoc
	err = m.messages.FindOne(ctx, bson.M{"session_id": sessionID, "chat_jid": chatJID, "id": id}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			err = ErrNotFound
		}
		return
	}

	mediaKey, err = decryptBytes(doc.MediaKey)
	if err != nil {
		return
	}
	fileSHA256, err = decryptBytes(doc.FileSHA256)
	if err != nil {
		return
	}
	fileEncSHA256, err = decryptBytes(doc.FileEncSHA256)
	if err != nil {
		return
	}
	return doc.MediaType, doc.Filename, doc.URL, mediaKey, fileSHA256, fileEncSHA256, doc.FileLength, nil
}

// --- Session registry (tenant records) ---

type sessionDoc struct {
	ID        string        `bson:"_id"`
	JID       string        `bson:"jid,omitempty"`
	Status    SessionStatus `bson:"status"`
	CreatedAt time.Time     `bson:"created_at"`
	UpdatedAt time.Time     `bson:"updated_at"`
}

func (m *MongoStore) CreateSessionRecord(ctx context.Context, sessionID string) error {
	now := time.Now()
	_, err := m.sessions.InsertOne(ctx, sessionDoc{
		ID: sessionID, Status: StatusPending, CreatedAt: now, UpdatedAt: now,
	})
	return err
}

func (m *MongoStore) SetSessionConnected(ctx context.Context, sessionID, jid string) error {
	_, err := m.sessions.UpdateOne(ctx, bson.M{"_id": sessionID}, bson.M{"$set": bson.M{
		"jid": jid, "status": StatusConnected, "updated_at": time.Now(),
	}})
	return err
}

func (m *MongoStore) SetSessionStatus(ctx context.Context, sessionID string, status SessionStatus) error {
	_, err := m.sessions.UpdateOne(ctx, bson.M{"_id": sessionID}, bson.M{"$set": bson.M{
		"status": status, "updated_at": time.Now(),
	}})
	return err
}

func (m *MongoStore) ListConnectedSessions(ctx context.Context) ([]sessionDoc, error) {
	cur, err := m.sessions.Find(ctx, bson.M{"status": StatusConnected})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var docs []sessionDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	return docs, nil
}
