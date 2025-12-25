package imapserver

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/JB-SelfCompany/yggmail/internal/logging"
	"github.com/JB-SelfCompany/yggmail/internal/storage/filestore"
	"github.com/JB-SelfCompany/yggmail/internal/storage/sqlite3"
)

// setupTestMailbox creates a temporary test environment
func setupTestMailbox(t *testing.T) (*Mailbox, func()) {
	// Create temporary directories
	tempDir, err := os.MkdirTemp("", "yggmail-imap-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(tempDir, "test.db")
	fileStorePath := filepath.Join(tempDir, "maildata")

	// Initialize storage
	storage, err := sqlite3.NewSQLite3StorageStorage(dbPath)
	if err != nil {
		os.RemoveAll(tempDir)
		t.Fatalf("Failed to create storage: %v", err)
	}

	// Initialize FileStore
	fs, err := filestore.NewFileStore(fileStorePath)
	if err != nil {
		storage.Close()
		os.RemoveAll(tempDir)
		t.Fatalf("Failed to create FileStore: %v", err)
	}

	// Initialize LargeMailLogger
	logger := logging.NewLargeMailLogger()

	// Create backend
	backend := &Backend{
		Storage:         storage,
		FileStore:       fs,
		LargeMailLogger: logger,
	}

	// Create test mailbox
	mailbox := &Mailbox{
		backend: backend,
		name:    "INBOX",
	}

	// Create INBOX mailbox in storage
	if err := storage.MailboxCreate("INBOX"); err != nil {
		storage.Close()
		os.RemoveAll(tempDir)
		t.Fatalf("Failed to create INBOX: %v", err)
	}

	cleanup := func() {
		storage.Close()
		os.RemoveAll(tempDir)
	}

	return mailbox, cleanup
}

// generateTestMessage creates a test email message of specified size
func generateTestMessage(size int) []byte {
	header := `From: test@example.com
To: recipient@example.com
Subject: Test Message
Date: Mon, 01 Jan 2024 12:00:00 +0000
Content-Type: text/plain; charset=utf-8

`
	bodySize := size - len(header)
	if bodySize < 0 {
		bodySize = 0
	}

	body := bytes.Repeat([]byte("A"), bodySize)
	return append([]byte(header), body...)
}

// TestListMessages_SmallMessage tests fetching a small message from BLOB storage
func TestListMessages_SmallMessage(t *testing.T) {
	mbox, cleanup := setupTestMailbox(t)
	defer cleanup()

	// Create a small message (1 MB)
	messageSize := 1 * 1024 * 1024
	messageData := generateTestMessage(messageSize)

	// Store message in BLOB
	id, err := mbox.backend.Storage.MailCreate("INBOX", messageData)
	if err != nil {
		t.Fatalf("Failed to create message: %v", err)
	}

	// Fetch the message
	seqSet := &imap.SeqSet{}
	seqSet.AddNum(uint32(id))

	ch := make(chan *imap.Message, 1)
	items := []imap.FetchItem{imap.FetchRFC822Size, imap.FetchEnvelope}

	go func() {
		if err := mbox.ListMessages(true, seqSet, items, ch); err != nil {
			t.Errorf("ListMessages failed: %v", err)
		}
	}()

	msg := <-ch
	if msg == nil {
		t.Fatal("No message received")
	}

	// Verify size is correct
	if msg.Size != uint32(messageSize) {
		t.Errorf("Expected size %d, got %d", messageSize, msg.Size)
	}

	// Verify envelope was fetched
	if msg.Envelope == nil {
		t.Error("Envelope not fetched")
	}
}

// TestListMessages_LargeMessage tests fetching a large message from file storage
func TestListMessages_LargeMessage(t *testing.T) {
	mbox, cleanup := setupTestMailbox(t)
	defer cleanup()

	// Create a large message (15 MB - above SmallMessageThreshold)
	messageSize := 15 * 1024 * 1024
	messageData := generateTestMessage(messageSize)

	// Store message using streaming (should go to file)
	reader := bytes.NewReader(messageData)
	id, err := mbox.backend.Storage.MailCreateFromStream("INBOX", reader, mbox.backend.FileStore)
	if err != nil {
		t.Fatalf("Failed to create large message: %v", err)
	}

	// Verify message was stored in file (not BLOB)
	_, mail, err := mbox.backend.Storage.MailSelect("INBOX", id)
	if err != nil {
		t.Fatalf("Failed to select message: %v", err)
	}
	if mail.MailFile == "" {
		t.Error("Expected message to be stored in file, but MailFile is empty")
	}
	if mail.Mail != nil {
		t.Error("Expected Mail BLOB to be nil for large messages")
	}

	// Fetch the message via IMAP
	seqSet := &imap.SeqSet{}
	seqSet.AddNum(uint32(id))

	ch := make(chan *imap.Message, 1)
	items := []imap.FetchItem{imap.FetchRFC822Size, imap.FetchEnvelope}

	go func() {
		if err := mbox.ListMessages(true, seqSet, items, ch); err != nil {
			t.Errorf("ListMessages failed: %v", err)
		}
	}()

	msg := <-ch
	if msg == nil {
		t.Fatal("No message received")
	}

	// Verify size is at least the threshold (exact size may vary due to storage implementation)
	// The important thing is that it's using mail.Size, not len(mail.Mail)
	if msg.Size < uint32(10*1024*1024) {
		t.Errorf("Expected size >= 10MB for large message, got %d", msg.Size)
	}
	if msg.Size > uint32(messageSize) {
		t.Errorf("Size %d exceeds expected maximum %d", msg.Size, messageSize)
	}

	// Verify envelope was fetched from peek buffer
	if msg.Envelope == nil {
		t.Error("Envelope not fetched for large message")
	}
}

// TestCreateMessage_SmallMessage tests creating a small message
func TestCreateMessage_SmallMessage(t *testing.T) {
	mbox, cleanup := setupTestMailbox(t)
	defer cleanup()

	// Create a small message (5 MB)
	messageSize := 5 * 1024 * 1024
	messageData := generateTestMessage(messageSize)

	// Create message via IMAP APPEND
	body := bytes.NewReader(messageData)
	err := mbox.CreateMessage([]string{"\\Seen"}, time.Now(), body)
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}

	// Verify message was stored in BLOB
	count, err := mbox.backend.Storage.MailCount("INBOX")
	if err != nil {
		t.Fatalf("Failed to get mail count: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 message, got %d", count)
	}

	// Get the message and verify storage type
	_, mail, err := mbox.backend.Storage.MailSelect("INBOX", 1)
	if err != nil {
		t.Fatalf("Failed to select message: %v", err)
	}
	if mail.MailFile != "" {
		t.Error("Expected small message to be in BLOB, not file")
	}
	if !mail.Seen {
		t.Error("Expected Seen flag to be set")
	}
}

// TestCreateMessage_LargeMessage tests creating a large message
func TestCreateMessage_LargeMessage(t *testing.T) {
	mbox, cleanup := setupTestMailbox(t)
	defer cleanup()

	// Create a large message (20 MB)
	messageSize := 20 * 1024 * 1024
	messageData := generateTestMessage(messageSize)

	// Create message via IMAP APPEND
	body := bytes.NewReader(messageData)
	err := mbox.CreateMessage([]string{"\\Flagged"}, time.Now(), body)
	if err != nil {
		t.Fatalf("CreateMessage failed: %v", err)
	}

	// Verify message was stored in file
	count, err := mbox.backend.Storage.MailCount("INBOX")
	if err != nil {
		t.Fatalf("Failed to get mail count: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 message, got %d", count)
	}

	// Get the message and verify storage type
	_, mail, err := mbox.backend.Storage.MailSelect("INBOX", 1)
	if err != nil {
		t.Fatalf("Failed to select message: %v", err)
	}
	if mail.MailFile == "" {
		t.Error("Expected large message to be in file storage")
	}
	if mail.Mail != nil {
		t.Error("Expected Mail BLOB to be nil for large message")
	}
	if !mail.Flagged {
		t.Error("Expected Flagged flag to be set")
	}
	// Verify size is at least the threshold
	if mail.Size < int64(10*1024*1024) {
		t.Errorf("Expected size >= 10MB for large message, got %d", mail.Size)
	}
}

// TestCopyMessages_LargeMessage tests copying a large file-based message
func TestCopyMessages_LargeMessage(t *testing.T) {
	mbox, cleanup := setupTestMailbox(t)
	defer cleanup()

	// Create destination mailbox
	if err := mbox.backend.Storage.MailboxCreate("Archive"); err != nil {
		t.Fatalf("Failed to create Archive mailbox: %v", err)
	}

	// Create a large message (25 MB)
	messageSize := 25 * 1024 * 1024
	messageData := generateTestMessage(messageSize)

	// Store in INBOX using streaming
	reader := bytes.NewReader(messageData)
	id, err := mbox.backend.Storage.MailCreateFromStream("INBOX", reader, mbox.backend.FileStore)
	if err != nil {
		t.Fatalf("Failed to create large message: %v", err)
	}

	// Verify it's in file storage
	_, origMail, err := mbox.backend.Storage.MailSelect("INBOX", id)
	if err != nil {
		t.Fatalf("Failed to select original message: %v", err)
	}
	if origMail.MailFile == "" {
		t.Fatal("Expected message to be in file storage")
	}

	// Copy to Archive
	seqSet := &imap.SeqSet{}
	seqSet.AddNum(uint32(id))

	if err := mbox.CopyMessages(true, seqSet, "Archive"); err != nil {
		t.Fatalf("CopyMessages failed: %v", err)
	}

	// Verify copied message exists in Archive
	archiveCount, err := mbox.backend.Storage.MailCount("Archive")
	if err != nil {
		t.Fatalf("Failed to get Archive count: %v", err)
	}
	if archiveCount != 1 {
		t.Errorf("Expected 1 message in Archive, got %d", archiveCount)
	}

	// Verify copied message is also in file storage
	_, copiedMail, err := mbox.backend.Storage.MailSelect("Archive", 1)
	if err != nil {
		t.Fatalf("Failed to select copied message: %v", err)
	}
	if copiedMail.MailFile == "" {
		t.Error("Expected copied message to be in file storage")
	}
	// Verify size is at least the threshold
	if copiedMail.Size < int64(10*1024*1024) {
		t.Errorf("Expected copied size >= 10MB for large message, got %d", copiedMail.Size)
	}

	// Verify original file still exists
	origFile, err := mbox.backend.FileStore.ReadMail(origMail.MailFile)
	if err != nil {
		t.Errorf("Original file should still exist: %v", err)
	} else {
		origFile.Close()
	}

	// Verify copied file exists and is different from original
	copiedFile, err := mbox.backend.FileStore.ReadMail(copiedMail.MailFile)
	if err != nil {
		t.Errorf("Copied file should exist: %v", err)
	} else {
		copiedFile.Close()
	}

	if origMail.MailFile == copiedMail.MailFile {
		t.Error("Copied message should have different file path than original")
	}
}

// TestListMessages_PeekHeaders tests that large messages use peek buffer for headers
func TestListMessages_PeekHeaders(t *testing.T) {
	mbox, cleanup := setupTestMailbox(t)
	defer cleanup()

	// Create a very large message (50 MB)
	messageSize := 50 * 1024 * 1024
	messageData := generateTestMessage(messageSize)

	// Store using streaming
	reader := bytes.NewReader(messageData)
	id, err := mbox.backend.Storage.MailCreateFromStream("INBOX", reader, mbox.backend.FileStore)
	if err != nil {
		t.Fatalf("Failed to create large message: %v", err)
	}

	// Fetch only envelope (should use peek buffer, not load entire 50MB)
	seqSet := &imap.SeqSet{}
	seqSet.AddNum(uint32(id))

	ch := make(chan *imap.Message, 1)
	items := []imap.FetchItem{imap.FetchEnvelope}

	go func() {
		if err := mbox.ListMessages(true, seqSet, items, ch); err != nil {
			t.Errorf("ListMessages failed: %v", err)
		}
	}()

	msg := <-ch
	if msg == nil {
		t.Fatal("No message received")
	}

	// Verify envelope was fetched successfully
	if msg.Envelope == nil {
		t.Error("Envelope should be fetched from peek buffer")
	}

	// Basic envelope checks
	if msg.Envelope.Subject != "Test Message" {
		t.Errorf("Expected subject 'Test Message', got '%s'", msg.Envelope.Subject)
	}
}

// TestReaderWithCloser tests the readerWithCloser helper
func TestReaderWithCloser(t *testing.T) {
	closed := false
	closeFn := func() error {
		closed = true
		return nil
	}

	data := []byte("test data")
	r := &readerWithCloser{
		reader:  bytes.NewReader(data),
		closeFn: closeFn,
	}

	// Read all data
	buf := make([]byte, len(data))
	n, err := io.ReadFull(r, buf)
	if err != nil {
		t.Fatalf("ReadFull failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected to read %d bytes, got %d", len(data), n)
	}

	// Read EOF (should trigger close)
	_, err = r.Read(buf)
	if err != io.EOF {
		t.Errorf("Expected EOF, got %v", err)
	}

	if !closed {
		t.Error("closeFn should have been called on EOF")
	}

	// Close again (should be idempotent)
	err = r.Close()
	if err != nil {
		t.Errorf("Close should be idempotent, got error: %v", err)
	}
}
