package e2e

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/JB-SelfCompany/yggmail/internal/imapserver"
	"github.com/JB-SelfCompany/yggmail/internal/logging"
	"github.com/JB-SelfCompany/yggmail/internal/smtpsender"
	"github.com/JB-SelfCompany/yggmail/internal/smtpserver"
	"github.com/JB-SelfCompany/yggmail/internal/storage/filestore"
	"github.com/JB-SelfCompany/yggmail/internal/storage/sqlite3"
	"github.com/JB-SelfCompany/yggmail/internal/storage/types"
)

// TestNode represents a complete Yggmail node for testing
type TestNode struct {
	Name            string
	TempDir         string
	Storage         *sqlite3.SQLite3Storage
	FileStore       *filestore.FileStore
	LargeMailLogger *logging.LargeMailLogger
	SMTPBackend     *smtpserver.Backend
	IMAPBackend     *imapserver.Backend
	Queues          *smtpsender.Queues
	t               *testing.T
}

// setupTestNode creates a complete test node with all components
func setupTestNode(t *testing.T, name string) *TestNode {
	// Create temporary directory
	tempDir, err := os.MkdirTemp("", fmt.Sprintf("yggmail-e2e-%s-*", name))
	if err != nil {
		t.Fatalf("Failed to create temp dir for %s: %v", name, err)
	}

	dbPath := filepath.Join(tempDir, "test.db")
	fileStorePath := filepath.Join(tempDir, "maildata")

	// Initialize storage
	storage, err := sqlite3.NewSQLite3StorageStorage(dbPath)
	if err != nil {
		os.RemoveAll(tempDir)
		t.Fatalf("Failed to create storage for %s: %v", name, err)
	}

	// Initialize FileStore
	fs, err := filestore.NewFileStore(fileStorePath)
	if err != nil {
		storage.Close()
		os.RemoveAll(tempDir)
		t.Fatalf("Failed to create FileStore for %s: %v", name, err)
	}

	// Initialize LargeMailLogger
	logger := logging.NewLargeMailLogger()

	// Create mailboxes
	if err := storage.MailboxCreate("INBOX"); err != nil {
		storage.Close()
		os.RemoveAll(tempDir)
		t.Fatalf("Failed to create INBOX for %s: %v", name, err)
	}
	if err := storage.MailboxCreate("Outbox"); err != nil {
		storage.Close()
		os.RemoveAll(tempDir)
		t.Fatalf("Failed to create Outbox for %s: %v", name, err)
	}

	// Create SMTP backend
	smtpBackend := &smtpserver.Backend{
		Storage:         storage,
		FileStore:       fs,
		LargeMailLogger: logger,
	}

	// Create IMAP backend
	imapBackend := &imapserver.Backend{
		Storage:         storage,
		FileStore:       fs,
		LargeMailLogger: logger,
	}

	// Create sender queues (simplified for testing)
	queues := &smtpsender.Queues{
		Storage:         storage,
		FileStore:       fs,
		LargeMailLogger: logger,
	}

	return &TestNode{
		Name:            name,
		TempDir:         tempDir,
		Storage:         storage,
		FileStore:       fs,
		LargeMailLogger: logger,
		SMTPBackend:     smtpBackend,
		IMAPBackend:     imapBackend,
		Queues:          queues,
		t:               t,
	}
}

// Cleanup cleans up test node resources
func (n *TestNode) Cleanup() {
	if n.Storage != nil {
		n.Storage.Close()
	}
	if n.TempDir != "" {
		os.RemoveAll(n.TempDir)
	}
}

// generateTestMail creates a test email message with specified size
func generateTestMail(size int, subject string) []byte {
	header := fmt.Sprintf(`From: sender@yggmail
To: recipient@yggmail
Subject: %s
Date: %s
Content-Type: text/plain; charset=utf-8
MIME-Version: 1.0

`, subject, time.Now().Format(time.RFC1123Z))

	bodySize := size - len(header)
	if bodySize < 0 {
		bodySize = 100
	}

	// Generate random body data to be more realistic
	body := make([]byte, bodySize)
	rand.Read(body)
	// Replace with printable characters for valid email
	for i := range body {
		body[i] = 'A' + (body[i] % 26)
	}

	return append([]byte(header), body...)
}

// TestE2E_Send50MBMail tests sending a 50MB mail between nodes
func TestE2E_Send50MBMail(t *testing.T) {
	sender := setupTestNode(t, "sender")
	defer sender.Cleanup()

	receiver := setupTestNode(t, "receiver")
	defer receiver.Cleanup()

	// Generate 50 MB message
	messageSize := 50 * 1024 * 1024
	t.Logf("Generating %d MB test message...", messageSize/(1024*1024))
	messageData := generateTestMail(messageSize, "Test 50MB Message")

	startTime := time.Now()

	// Simulate SMTP receive on sender (queuing for delivery)
	t.Log("Sender: Queuing message for delivery...")
	reader := bytes.NewReader(messageData)
	mailID, err := sender.Storage.MailCreateFromStream("Outbox", reader, sender.FileStore)
	if err != nil {
		t.Fatalf("Failed to queue message: %v", err)
	}

	// Verify message was stored in file (not BLOB)
	_, mail, err := sender.Storage.MailSelect("Outbox", mailID)
	if err != nil {
		t.Fatalf("Failed to select queued message: %v", err)
	}
	if mail.MailFile == "" {
		t.Error("Expected 50MB message to be stored in file, but MailFile is empty")
	}
	if mail.Size < int64(types.SmallMessageThreshold) {
		t.Errorf("Expected size >= 10MB, got %d", mail.Size)
	}
	t.Logf("Sender: Message stored in file: %s (size: %d bytes)", mail.MailFile, mail.Size)

	// Simulate streaming transfer to receiver
	t.Log("Transferring message to receiver...")
	var mailReader io.Reader
	if mail.MailFile != "" {
		file, err := sender.FileStore.ReadMail(mail.MailFile)
		if err != nil {
			t.Fatalf("Failed to open mail file for transfer: %v", err)
		}
		defer file.Close()
		mailReader = file
	} else {
		mailReader = bytes.NewReader(mail.Mail)
	}

	// Receiver stores the message
	receivedID, err := receiver.Storage.MailCreateFromStream("INBOX", mailReader, receiver.FileStore)
	if err != nil {
		t.Fatalf("Receiver failed to store message: %v", err)
	}

	// Verify received message
	_, receivedMail, err := receiver.Storage.MailSelect("INBOX", receivedID)
	if err != nil {
		t.Fatalf("Failed to select received message: %v", err)
	}
	if receivedMail.MailFile == "" {
		t.Error("Expected received 50MB message to be in file storage")
	}
	if receivedMail.Size != mail.Size {
		t.Errorf("Size mismatch: sent %d, received %d", mail.Size, receivedMail.Size)
	}

	duration := time.Since(startTime)
	speed := float64(mail.Size) / duration.Seconds() / (1024 * 1024) // MB/s
	t.Logf("SUCCESS: 50MB message transferred in %v (%.2f MB/s)", duration, speed)

	// Verify files exist and can be read
	senderFile, err := sender.FileStore.ReadMail(mail.MailFile)
	if err != nil {
		t.Errorf("Sender file should exist: %v", err)
	} else {
		senderFile.Close()
	}

	receiverFile, err := receiver.FileStore.ReadMail(receivedMail.MailFile)
	if err != nil {
		t.Errorf("Receiver file should exist: %v", err)
	} else {
		receiverFile.Close()
	}

	t.Log("✓ E2E 50MB test passed")
}

// TestE2E_Send100MBMail tests sending a 100MB mail (maximum size)
func TestE2E_Send100MBMail(t *testing.T) {
	sender := setupTestNode(t, "sender-100mb")
	defer sender.Cleanup()

	receiver := setupTestNode(t, "receiver-100mb")
	defer receiver.Cleanup()

	// Generate 100 MB message
	messageSize := 100 * 1024 * 1024
	t.Logf("Generating %d MB test message...", messageSize/(1024*1024))
	messageData := generateTestMail(messageSize, "Test 100MB Message - Maximum Size")

	startTime := time.Now()

	// Queue on sender
	t.Log("Sender: Queuing 100MB message...")
	reader := bytes.NewReader(messageData)
	mailID, err := sender.Storage.MailCreateFromStream("Outbox", reader, sender.FileStore)
	if err != nil {
		t.Fatalf("Failed to queue 100MB message: %v", err)
	}

	// Verify file storage
	_, mail, err := sender.Storage.MailSelect("Outbox", mailID)
	if err != nil {
		t.Fatalf("Failed to select queued message: %v", err)
	}
	if mail.MailFile == "" {
		t.Error("Expected 100MB message to be in file storage")
	}
	t.Logf("Sender: Message stored (size: %.2f MB)", float64(mail.Size)/(1024*1024))

	// Transfer to receiver with chunked reading
	t.Log("Transferring 100MB message in chunks...")
	var mailReader io.Reader
	if mail.MailFile != "" {
		file, err := sender.FileStore.ReadMail(mail.MailFile)
		if err != nil {
			t.Fatalf("Failed to open mail file: %v", err)
		}
		defer file.Close()
		mailReader = file
	} else {
		mailReader = bytes.NewReader(mail.Mail)
	}

	// Simulate chunked transfer with progress logging
	progressReader := &progressReader{
		reader: mailReader,
		total:  mail.Size,
		onProgress: func(bytesRead int64) {
			percent := float64(bytesRead) / float64(mail.Size) * 100
			if bytesRead%(10*1024*1024) == 0 || percent >= 99.9 {
				t.Logf("  Transfer progress: %.1f%% (%d / %d bytes)", percent, bytesRead, mail.Size)
			}
		},
	}

	receivedID, err := receiver.Storage.MailCreateFromStream("INBOX", progressReader, receiver.FileStore)
	if err != nil {
		t.Fatalf("Receiver failed to store 100MB message: %v", err)
	}

	// Verify received message
	_, receivedMail, err := receiver.Storage.MailSelect("INBOX", receivedID)
	if err != nil {
		t.Fatalf("Failed to select received message: %v", err)
	}
	if receivedMail.MailFile == "" {
		t.Error("Expected received 100MB message in file storage")
	}
	if receivedMail.Size != mail.Size {
		t.Errorf("Size mismatch: sent %d, received %d", mail.Size, receivedMail.Size)
	}

	duration := time.Since(startTime)
	speed := float64(mail.Size) / duration.Seconds() / (1024 * 1024) // MB/s
	t.Logf("SUCCESS: 100MB message transferred in %v (%.2f MB/s)", duration, speed)
	t.Log("✓ E2E 100MB test passed")
}

// TestE2E_SmallMessageStillWorks tests backward compatibility with small messages
func TestE2E_SmallMessageStillWorks(t *testing.T) {
	node := setupTestNode(t, "small-msg")
	defer node.Cleanup()

	// Generate small message (1 MB)
	messageSize := 1 * 1024 * 1024
	messageData := generateTestMail(messageSize, "Small 1MB Message")

	// Store via SMTP
	mailID, err := node.Storage.MailCreate("INBOX", messageData)
	if err != nil {
		t.Fatalf("Failed to create small message: %v", err)
	}

	// Verify it's in BLOB, not file
	_, mail, err := node.Storage.MailSelect("INBOX", mailID)
	if err != nil {
		t.Fatalf("Failed to select message: %v", err)
	}
	if mail.MailFile != "" {
		t.Error("Expected small message in BLOB, not file")
	}
	if mail.Mail == nil {
		t.Error("Expected Mail BLOB to contain data for small message")
	}
	if mail.Size != int64(messageSize) {
		t.Errorf("Size mismatch: expected %d, got %d", messageSize, mail.Size)
	}

	t.Log("✓ Small message backward compatibility test passed")
}

// TestE2E_ThresholdBoundary tests messages around the 10MB threshold
func TestE2E_ThresholdBoundary(t *testing.T) {
	node := setupTestNode(t, "threshold")
	defer node.Cleanup()

	testCases := []struct {
		name        string
		size        int
		expectFile  bool
		description string
	}{
		{"9MB", 9 * 1024 * 1024, false, "Just under threshold - BLOB"},
		{"10MB", 10 * 1024 * 1024, true, "At threshold - File"},
		{"11MB", 11 * 1024 * 1024, true, "Just over threshold - File"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			messageData := generateTestMail(tc.size, fmt.Sprintf("Threshold Test - %s", tc.name))
			reader := bytes.NewReader(messageData)

			mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
			if err != nil {
				t.Fatalf("Failed to create %s message: %v", tc.name, err)
			}

			_, mail, err := node.Storage.MailSelect("INBOX", mailID)
			if err != nil {
				t.Fatalf("Failed to select %s message: %v", tc.name, err)
			}

			if tc.expectFile {
				if mail.MailFile == "" {
					t.Errorf("%s: Expected file storage, got BLOB", tc.description)
				}
				if mail.Mail != nil {
					t.Errorf("%s: Expected Mail BLOB to be nil", tc.description)
				}
			} else {
				if mail.MailFile != "" {
					t.Errorf("%s: Expected BLOB storage, got file", tc.description)
				}
				if mail.Mail == nil {
					t.Errorf("%s: Expected Mail BLOB to have data", tc.description)
				}
			}

			t.Logf("✓ %s - Storage type correct", tc.description)

			// Clean up for next test
			if err := node.Storage.MailDelete("INBOX", mailID); err != nil {
				t.Errorf("Failed to delete test message: %v", err)
			}
		})
	}
}

// progressReader wraps an io.Reader to report progress
type progressReader struct {
	reader     io.Reader
	total      int64
	bytesRead  int64
	onProgress func(bytesRead int64)
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.reader.Read(p)
	pr.bytesRead += int64(n)
	if pr.onProgress != nil && n > 0 {
		pr.onProgress(pr.bytesRead)
	}
	return
}

// TestE2E_ParallelLargeMessages tests concurrent transfer of multiple large messages
func TestE2E_ParallelLargeMessages(t *testing.T) {
	const numMessages = 10
	const messageSize = 15 * 1024 * 1024 // 15 MB each

	sender := setupTestNode(t, "parallel-sender")
	defer sender.Cleanup()

	receiver := setupTestNode(t, "parallel-receiver")
	defer receiver.Cleanup()

	t.Logf("Starting parallel transfer of %d messages (%d MB each)...", numMessages, messageSize/(1024*1024))

	var wg sync.WaitGroup
	errors := make(chan error, numMessages)
	startTime := time.Now()

	for i := 0; i < numMessages; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			// Generate unique message
			messageData := generateTestMail(messageSize, fmt.Sprintf("Parallel Test Message #%d", index))

			// Queue on sender
			reader := bytes.NewReader(messageData)
			mailID, err := sender.Storage.MailCreateFromStream("Outbox", reader, sender.FileStore)
			if err != nil {
				errors <- fmt.Errorf("Message %d: queue failed: %v", index, err)
				return
			}

			// Read from sender
			_, mail, err := sender.Storage.MailSelect("Outbox", mailID)
			if err != nil {
				errors <- fmt.Errorf("Message %d: select failed: %v", index, err)
				return
			}

			// Transfer to receiver
			var mailReader io.Reader
			if mail.MailFile != "" {
				file, err := sender.FileStore.ReadMail(mail.MailFile)
				if err != nil {
					errors <- fmt.Errorf("Message %d: open file failed: %v", index, err)
					return
				}
				defer file.Close()
				mailReader = file
			} else {
				mailReader = bytes.NewReader(mail.Mail)
			}

			receivedID, err := receiver.Storage.MailCreateFromStream("INBOX", mailReader, receiver.FileStore)
			if err != nil {
				errors <- fmt.Errorf("Message %d: receive failed: %v", index, err)
				return
			}

			// Verify
			_, receivedMail, err := receiver.Storage.MailSelect("INBOX", receivedID)
			if err != nil {
				errors <- fmt.Errorf("Message %d: verify failed: %v", index, err)
				return
			}

			if receivedMail.Size != mail.Size {
				errors <- fmt.Errorf("Message %d: size mismatch", index)
				return
			}

			t.Logf("  Message #%d transferred successfully (%d bytes)", index, receivedMail.Size)
		}(i)
	}

	wg.Wait()
	close(errors)

	duration := time.Since(startTime)
	totalBytes := int64(numMessages * messageSize)
	speed := float64(totalBytes) / duration.Seconds() / (1024 * 1024)

	// Check for errors
	var errorList []error
	for err := range errors {
		errorList = append(errorList, err)
	}

	if len(errorList) > 0 {
		t.Errorf("Encountered %d errors during parallel transfer:", len(errorList))
		for _, err := range errorList {
			t.Error("  ", err)
		}
		t.FailNow()
	}

	t.Logf("SUCCESS: %d messages (%d MB total) transferred in %v (%.2f MB/s)",
		numMessages, totalBytes/(1024*1024), duration, speed)
	t.Log("✓ Parallel large messages test passed")
}

// TestE2E_MemoryFootprint measures memory usage during large file transfer
func TestE2E_MemoryFootprint(t *testing.T) {
	node := setupTestNode(t, "memory-test")
	defer node.Cleanup()

	// Generate 50 MB message
	messageSize := 50 * 1024 * 1024
	messageData := generateTestMail(messageSize, "Memory Footprint Test")

	// Force GC before measurement
	var m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m1)

	t.Logf("Memory before transfer: Alloc=%v MB, TotalAlloc=%v MB",
		m1.Alloc/(1024*1024), m1.TotalAlloc/(1024*1024))

	// Perform streaming transfer
	reader := bytes.NewReader(messageData)
	mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
	if err != nil {
		t.Fatalf("Failed to create message: %v", err)
	}

	// Measure after transfer
	var m2 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m2)

	allocDiff := int64(m2.Alloc - m1.Alloc)
	t.Logf("Memory after transfer: Alloc=%v MB, TotalAlloc=%v MB",
		m2.Alloc/(1024*1024), m2.TotalAlloc/(1024*1024))
	t.Logf("Memory increase: %v MB", allocDiff/(1024*1024))

	// Verify message was stored
	_, mail, err := node.Storage.MailSelect("INBOX", mailID)
	if err != nil {
		t.Fatalf("Failed to select message: %v", err)
	}
	if mail.MailFile == "" {
		t.Error("Expected message in file storage")
	}

	// Target: memory increase should be < 5 MB (ideally ~2 MB for buffers)
	const maxMemoryIncrease = 5 * 1024 * 1024 // 5 MB tolerance
	if allocDiff > maxMemoryIncrease {
		t.Errorf("Memory footprint too large: %d MB (expected < 5 MB)",
			allocDiff/(1024*1024))
		t.Log("⚠ Memory footprint exceeds target - buffer optimization may be needed")
	} else {
		t.Logf("✓ Memory footprint within acceptable range: %d MB", allocDiff/(1024*1024))
	}
}
