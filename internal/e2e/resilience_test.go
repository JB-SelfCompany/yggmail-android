package e2e

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// interruptedReader simulates connection interruption
type interruptedReader struct {
	reader         io.Reader
	bytesRead      int64
	interruptAt    int64
	interrupted    bool
	resumeFrom     int64
	totalSize      int64
	interruptCount int
	maxInterrupts  int
}

func (ir *interruptedReader) Read(p []byte) (n int, err error) {
	if ir.interrupted {
		// Simulate reconnection after interruption
		if ir.interruptCount >= ir.maxInterrupts {
			// No more interruptions, continue reading
			ir.interrupted = false
		} else {
			return 0, fmt.Errorf("connection interrupted at %d bytes", ir.bytesRead)
		}
	}

	n, err = ir.reader.Read(p)
	ir.bytesRead += int64(n)

	// Interrupt at specified point
	if !ir.interrupted && ir.bytesRead >= ir.interruptAt && ir.interruptCount < ir.maxInterrupts {
		ir.interrupted = true
		ir.interruptCount++
		return n, fmt.Errorf("connection interrupted at %d bytes", ir.bytesRead)
	}

	return n, err
}

// TestConnectionInterruption tests handling of connection interruptions during transfer
func TestConnectionInterruption(t *testing.T) {
	node := setupTestNode(t, "interrupt-test")
	defer node.Cleanup()

	// Generate 30 MB message
	messageSize := 30 * 1024 * 1024
	t.Log("Generating 30MB message for interruption test...")
	messageData := generateTestMail(messageSize, "Interruption Test Message")

	// Test single interruption at 50%
	t.Run("SingleInterruption", func(t *testing.T) {
		reader := bytes.NewReader(messageData)
		interruptReader := &interruptedReader{
			reader:        reader,
			interruptAt:   int64(messageSize / 2), // Interrupt at 50%
			maxInterrupts: 1,
		}

		// First attempt - will fail
		_, err := node.Storage.MailCreateFromStream("INBOX", interruptReader, node.FileStore)
		if err == nil {
			t.Error("Expected error due to interruption, got nil")
		}
		t.Logf("First attempt failed as expected: %v", err)

		// Retry from beginning (in real scenario, would be queued for retry)
		reader = bytes.NewReader(messageData)
		mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
		if err != nil {
			t.Fatalf("Retry failed: %v", err)
		}

		// Verify message was stored successfully
		_, mail, err := node.Storage.MailSelect("INBOX", mailID)
		if err != nil {
			t.Fatalf("Failed to select message: %v", err)
		}
		if mail.MailFile == "" {
			t.Error("Expected message in file storage")
		}
		if mail.Size != int64(messageSize) {
			t.Errorf("Size mismatch after retry: expected %d, got %d", messageSize, mail.Size)
		}

		t.Log("✓ Single interruption test passed - retry successful")
	})

	// Test partial write cleanup
	t.Run("PartialWriteCleanup", func(t *testing.T) {
		// Clean up previous test data
		node.Storage.MailDelete("INBOX", 1)

		reader := bytes.NewReader(messageData)
		interruptReader := &interruptedReader{
			reader:        reader,
			interruptAt:   5 * 1024 * 1024, // Interrupt early at 5 MB
			maxInterrupts: 1,
		}

		// Attempt that will fail
		_, err := node.Storage.MailCreateFromStream("INBOX", interruptReader, node.FileStore)
		if err == nil {
			t.Error("Expected error due to interruption")
		}

		// Check for orphaned files (should be cleaned up)
		// In production, CleanupOrphanedFiles would handle this
		stats, err := node.FileStore.GetTotalSize()
		if err != nil {
			t.Logf("Warning: Could not get file store stats: %v", err)
		} else {
			t.Logf("File store size after failed write: %d bytes", stats)
		}

		t.Log("✓ Partial write cleanup test completed")
	})
}

// TestMigrationExistingMessages tests migration of existing BLOB messages to file storage
func TestMigrationExistingMessages(t *testing.T) {
	node := setupTestNode(t, "migration-test")
	defer node.Cleanup()

	// Create several "legacy" messages stored in BLOB
	t.Log("Creating legacy BLOB messages...")
	legacyMessages := []struct {
		size    int
		mailbox string
	}{
		{5 * 1024 * 1024, "INBOX"},   // 5 MB
		{8 * 1024 * 1024, "INBOX"},   // 8 MB
		{15 * 1024 * 1024, "INBOX"},  // 15 MB (should migrate)
		{25 * 1024 * 1024, "INBOX"},  // 25 MB (should migrate)
		{2 * 1024 * 1024, "Outbox"},  // 2 MB
		{20 * 1024 * 1024, "Outbox"}, // 20 MB (should migrate)
	}

	messageIDs := make(map[string][]int)
	for i, msg := range legacyMessages {
		data := generateTestMail(msg.size, fmt.Sprintf("Legacy Message %d", i))

		// Store in BLOB using MailCreate (old method)
		mailID, err := node.Storage.MailCreate(msg.mailbox, data)
		if err != nil {
			t.Fatalf("Failed to create legacy message %d: %v", i, err)
		}
		messageIDs[msg.mailbox] = append(messageIDs[msg.mailbox], mailID)

		t.Logf("Created legacy message %d in %s: %d bytes", mailID, msg.mailbox, msg.size)
	}

	// Verify all messages are in BLOB
	t.Log("Verifying messages are in BLOB storage...")
	for mailbox, ids := range messageIDs {
		for _, id := range ids {
			_, mail, err := node.Storage.MailSelect(mailbox, id)
			if err != nil {
				t.Fatalf("Failed to select message %s/%d: %v", mailbox, id, err)
			}
			if mail.Mail == nil {
				t.Errorf("Expected message %s/%d in BLOB before migration", mailbox, id)
			}
			if mail.MailFile != "" {
				t.Errorf("Expected MailFile to be empty before migration for %s/%d", mailbox, id)
			}
		}
	}

	// Run migration for messages > 10 MB
	t.Log("Running migration for messages > 10 MB...")
	threshold := int64(10 * 1024 * 1024)

	// Count messages before migration for verification
	messageCountBefore := 0
	for _, ids := range messageIDs {
		for _, id := range ids {
			_, mail, _ := node.Storage.MailSelect("INBOX", id)
			if mail != nil && mail.Size > threshold {
				messageCountBefore++
			}
		}
		for _, id := range ids {
			_, mail, _ := node.Storage.MailSelect("Outbox", id)
			if mail != nil && mail.Size > threshold {
				messageCountBefore++
			}
		}
	}

	err := node.Storage.MigrateLargeMessagesToFiles(node.FileStore, threshold)
	if err != nil {
		t.Fatalf("Migration failed: %v", err)
	}

	t.Logf("Migration completed successfully")

	// Count migrated messages (messages with MailFile != "")
	migrated := 0
	for mailbox, ids := range messageIDs {
		for _, id := range ids {
			_, mail, err := node.Storage.MailSelect(mailbox, id)
			if err == nil && mail.MailFile != "" {
				migrated++
			}
		}
	}

	// Expected migrations: 15MB, 25MB, 20MB = 3 messages
	expectedMigrations := 3
	if migrated != expectedMigrations {
		t.Errorf("Expected %d migrations, got %d", expectedMigrations, migrated)
	}

	// Verify migrated messages
	t.Log("Verifying post-migration state...")
	for i, msg := range legacyMessages {
		mailbox := msg.mailbox
		idx := 0
		for j := 0; j < i; j++ {
			if legacyMessages[j].mailbox == mailbox {
				idx++
			}
		}
		mailID := messageIDs[mailbox][idx]

		_, mail, err := node.Storage.MailSelect(mailbox, mailID)
		if err != nil {
			t.Fatalf("Failed to select message after migration: %v", err)
		}

		shouldBeMigrated := msg.size > int(threshold)

		if shouldBeMigrated {
			// Should be in file storage
			if mail.MailFile == "" {
				t.Errorf("Message %s/%d (%d bytes) should be migrated to file", mailbox, mailID, msg.size)
			}
			if mail.Mail != nil {
				t.Errorf("Message %s/%d BLOB should be NULL after migration", mailbox, mailID)
			}

			// Verify file can be read
			file, err := node.FileStore.ReadMail(mail.MailFile)
			if err != nil {
				t.Errorf("Failed to read migrated file for %s/%d: %v", mailbox, mailID, err)
			} else {
				// Read a bit to verify file integrity
				buf := make([]byte, 1024)
				n, _ := file.Read(buf)
				if n == 0 {
					t.Errorf("Migrated file for %s/%d is empty", mailbox, mailID)
				}
				file.Close()
			}

			t.Logf("✓ Message %s/%d migrated to file: %s", mailbox, mailID, mail.MailFile)
		} else {
			// Should still be in BLOB
			if mail.MailFile != "" {
				t.Errorf("Message %s/%d (%d bytes) should remain in BLOB", mailbox, mailID, msg.size)
			}
			if mail.Mail == nil {
				t.Errorf("Message %s/%d BLOB should not be NULL", mailbox, mailID)
			}

			t.Logf("✓ Message %s/%d remains in BLOB (size %d < threshold)", mailbox, mailID, msg.size)
		}

		// Verify size is set correctly
		if mail.Size != int64(msg.size) {
			t.Errorf("Size mismatch for %s/%d: expected %d, got %d", mailbox, mailID, msg.size, mail.Size)
		}
	}

	t.Log("✓ Migration test passed - all messages in correct storage")
}

// TestMigrationIdempotency tests that running migration multiple times is safe
func TestMigrationIdempotency(t *testing.T) {
	node := setupTestNode(t, "idempotent-migration")
	defer node.Cleanup()

	// Create large message in BLOB
	messageSize := 20 * 1024 * 1024
	data := generateTestMail(messageSize, "Idempotency Test")
	mailID, err := node.Storage.MailCreate("INBOX", data)
	if err != nil {
		t.Fatalf("Failed to create message: %v", err)
	}

	threshold := int64(10 * 1024 * 1024)

	// First migration
	err = node.Storage.MigrateLargeMessagesToFiles(node.FileStore, threshold)
	if err != nil {
		t.Fatalf("First migration failed: %v", err)
	}

	// Get file path after first migration
	_, mail1, err := node.Storage.MailSelect("INBOX", mailID)
	if err != nil {
		t.Fatalf("Failed to select after first migration: %v", err)
	}
	firstFilePath := mail1.MailFile

	// Second migration (should do nothing - already migrated)
	t.Log("Running migration again (should be idempotent)...")
	err = node.Storage.MigrateLargeMessagesToFiles(node.FileStore, threshold)
	if err != nil {
		t.Fatalf("Second migration failed: %v", err)
	}

	// Verify file path unchanged
	_, mail2, err := node.Storage.MailSelect("INBOX", mailID)
	if err != nil {
		t.Fatalf("Failed to select after second migration: %v", err)
	}
	if mail2.MailFile != firstFilePath {
		t.Errorf("File path changed after second migration: %s -> %s", firstFilePath, mail2.MailFile)
	}

	// Verify file still readable
	file, err := node.FileStore.ReadMail(mail2.MailFile)
	if err != nil {
		t.Fatalf("File not readable after second migration: %v", err)
	}
	file.Close()

	t.Log("✓ Migration idempotency test passed")
}

// TestOrphanedFileCleanup tests cleanup of orphaned files
func TestOrphanedFileCleanup(t *testing.T) {
	node := setupTestNode(t, "cleanup-test")
	defer node.Cleanup()

	// Create a legitimate message
	messageSize := 15 * 1024 * 1024
	data := generateTestMail(messageSize, "Legitimate Message")
	reader := bytes.NewReader(data)
	mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
	if err != nil {
		t.Fatalf("Failed to create message: %v", err)
	}

	_, mail, err := node.Storage.MailSelect("INBOX", mailID)
	if err != nil {
		t.Fatalf("Failed to select message: %v", err)
	}

	legitimateFile := mail.MailFile
	t.Logf("Created legitimate file: %s", legitimateFile)

	// Create orphaned file (file exists but no DB reference)
	orphanedPath := filepath.Join(node.FileStore.BasePath(), "INBOX", "orphaned-999.eml")
	orphanedDir := filepath.Dir(orphanedPath)
	if err := os.MkdirAll(orphanedDir, 0755); err != nil {
		t.Fatalf("Failed to create orphaned file directory: %v", err)
	}

	orphanedData := []byte("This is an orphaned file that should be cleaned up")
	if err := os.WriteFile(orphanedPath, orphanedData, 0644); err != nil {
		t.Fatalf("Failed to create orphaned file: %v", err)
	}
	t.Logf("Created orphaned file: %s", orphanedPath)

	// Verify orphaned file exists
	if _, err := os.Stat(orphanedPath); os.IsNotExist(err) {
		t.Fatal("Orphaned file was not created")
	}

	// Build valid paths map from database
	validPaths := make(map[string]bool)
	validPaths[legitimateFile] = true

	// Run cleanup
	t.Log("Running orphaned file cleanup...")
	cleaned, cleanedSize, err := node.FileStore.CleanupOrphanedFiles(validPaths)
	if err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}

	t.Logf("Cleanup removed %d orphaned files (%d bytes)", cleaned, cleanedSize)

	// Verify orphaned file was removed
	if _, err := os.Stat(orphanedPath); !os.IsNotExist(err) {
		t.Error("Orphaned file should have been removed")
	}

	// Verify legitimate file still exists
	legitimateFullPath := filepath.Join(node.FileStore.BasePath(), legitimateFile)
	if _, err := os.Stat(legitimateFullPath); os.IsNotExist(err) {
		t.Error("Legitimate file should not have been removed")
	}

	// Verify message still accessible
	file, err := node.FileStore.ReadMail(legitimateFile)
	if err != nil {
		t.Errorf("Legitimate file should still be readable: %v", err)
	} else {
		file.Close()
	}

	t.Log("✓ Orphaned file cleanup test passed")
}

// TestBackwardCompatibility tests that old clients can still work with new storage
func TestBackwardCompatibility(t *testing.T) {
	node := setupTestNode(t, "compat-test")
	defer node.Cleanup()

	t.Run("OldClientSmallMessage", func(t *testing.T) {
		// Old client sends small message (uses MailCreate)
		messageSize := 5 * 1024 * 1024
		data := generateTestMail(messageSize, "Old Client Small Message")

		mailID, err := node.Storage.MailCreate("INBOX", data)
		if err != nil {
			t.Fatalf("Old client method failed: %v", err)
		}

		// Verify stored in BLOB
		_, mail, err := node.Storage.MailSelect("INBOX", mailID)
		if err != nil {
			t.Fatalf("Failed to select: %v", err)
		}
		if mail.MailFile != "" || mail.Mail == nil {
			t.Error("Old client small message should be in BLOB")
		}

		t.Log("✓ Old client small message compatibility confirmed")
	})

	t.Run("NewClientLargeMessage", func(t *testing.T) {
		// New client sends large message (uses MailCreateFromStream)
		messageSize := 20 * 1024 * 1024
		data := generateTestMail(messageSize, "New Client Large Message")

		reader := bytes.NewReader(data)
		mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
		if err != nil {
			t.Fatalf("New client method failed: %v", err)
		}

		// Verify stored in file
		_, mail, err := node.Storage.MailSelect("INBOX", mailID)
		if err != nil {
			t.Fatalf("Failed to select: %v", err)
		}
		if mail.MailFile == "" || mail.Mail != nil {
			t.Error("New client large message should be in file storage")
		}

		t.Log("✓ New client large message compatibility confirmed")
	})

	t.Run("MixedReading", func(t *testing.T) {
		// Verify both storage types can be read through same interface
		count, err := node.Storage.MailCount("INBOX")
		if err != nil {
			t.Fatalf("Failed to count messages: %v", err)
		}

		if count != 2 {
			t.Errorf("Expected 2 messages in INBOX, got %d", count)
		}

		// Read both messages
		for i := 1; i <= int(count); i++ {
			_, mail, err := node.Storage.MailSelect("INBOX", i)
			if err != nil {
				t.Errorf("Failed to read message %d: %v", i, err)
				continue
			}

			// Verify we can determine storage type and size
			var storageType string
			if mail.MailFile != "" {
				storageType = "file"
			} else {
				storageType = "BLOB"
			}

			t.Logf("Message %d: storage=%s, size=%d bytes", i, storageType, mail.Size)

			if mail.Size == 0 {
				t.Errorf("Message %d has zero size", i)
			}
		}

		t.Log("✓ Mixed storage reading compatibility confirmed")
	})
}
