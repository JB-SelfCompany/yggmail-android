package filestore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FileStore manages file-based storage for large email messages
type FileStore struct {
	basePath string
	mu       sync.RWMutex
}

// NewFileStore creates a new FileStore and initializes directory structure
// Creates subdirectories for different mailboxes (INBOX, Outbox, etc.)
func NewFileStore(basePath string) (*FileStore, error) {
	if basePath == "" {
		return nil, fmt.Errorf("basePath cannot be empty")
	}

	// Create base directory
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	fs := &FileStore{
		basePath: basePath,
	}

	// Create default mailbox directories
	defaultMailboxes := []string{"INBOX", "Outbox", "Sent", "Drafts", "Trash"}
	for _, mailbox := range defaultMailboxes {
		mailboxPath := filepath.Join(basePath, sanitizeMailboxName(mailbox))
		if err := os.MkdirAll(mailboxPath, 0755); err != nil {
			return nil, fmt.Errorf("failed to create mailbox directory %s: %w", mailbox, err)
		}
	}

	return fs, nil
}

// StoreMail stores an email message from a reader to a file
// Returns the file path where the message was stored and the actual bytes written
// Uses atomic write (temp file + rename) to prevent corruption
func (fs *FileStore) StoreMail(mailID int, mailbox string, reader io.Reader) (string, int64, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Sanitize mailbox name
	sanitized := sanitizeMailboxName(mailbox)
	mailboxPath := filepath.Join(fs.basePath, sanitized)

	// Create mailbox directory if it doesn't exist
	if err := os.MkdirAll(mailboxPath, 0755); err != nil {
		return "", 0, fmt.Errorf("failed to create mailbox directory: %w", err)
	}

	// Create temp file in same directory (for atomic rename)
	tempFile, err := os.CreateTemp(mailboxPath, fmt.Sprintf(".tmp_%d_*", mailID))
	if err != nil {
		return "", 0, fmt.Errorf("failed to create temp file: %w", err)
	}
	tempPath := tempFile.Name()

	// Clean up temp file on error
	defer func() {
		if tempFile != nil {
			tempFile.Close()
			os.Remove(tempPath)
		}
	}()

	// Stream data to temp file with buffering
	written, err := io.Copy(tempFile, reader)
	if err != nil {
		return "", 0, fmt.Errorf("failed to write data: %w", err)
	}

	// Log size for debugging
	fmt.Printf("[FileStore] StoreMail: mailID=%d, written=%d bytes (%.2f MB)\n",
		mailID, written, float64(written)/(1024*1024))

	// Sync to disk
	if err := tempFile.Sync(); err != nil {
		return "", 0, fmt.Errorf("failed to sync file: %w", err)
	}

	// Close before rename
	if err := tempFile.Close(); err != nil {
		return "", 0, fmt.Errorf("failed to close temp file: %w", err)
	}
	tempFile = nil // Prevent defer cleanup

	// Final file path
	finalPath := filepath.Join(mailboxPath, fmt.Sprintf("%d.eml", mailID))

	// Atomic rename
	if err := os.Rename(tempPath, finalPath); err != nil {
		return "", 0, fmt.Errorf("failed to rename temp file: %w", err)
	}

	// Return relative path from basePath
	relPath, err := filepath.Rel(fs.basePath, finalPath)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get relative path: %w", err)
	}

	return relPath, written, nil
}

// ReadMail opens and returns a reader for the specified mail file
// Caller is responsible for closing the returned ReadCloser
func (fs *FileStore) ReadMail(relPath string) (io.ReadCloser, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	// Validate path (prevent directory traversal)
	if strings.Contains(relPath, "..") {
		return nil, fmt.Errorf("invalid path: contains '..'")
	}

	fullPath := filepath.Join(fs.basePath, relPath)

	// Check if file exists
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("mail file not found: %s", relPath)
		}
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	file, err := os.Open(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}

	return file, nil
}

// DeleteMail deletes the specified mail file
func (fs *FileStore) DeleteMail(relPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Validate path
	if strings.Contains(relPath, "..") {
		return fmt.Errorf("invalid path: contains '..'")
	}

	fullPath := filepath.Join(fs.basePath, relPath)

	// Remove file
	if err := os.Remove(fullPath); err != nil {
		if os.IsNotExist(err) {
			return nil // Already deleted, not an error
		}
		return fmt.Errorf("failed to delete file: %w", err)
	}

	return nil
}

// MoveMail moves a mail file to a different mailbox with a new ID
// Returns the new relative path
func (fs *FileStore) MoveMail(oldRelPath string, newMailbox string, newMailID int) (string, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Validate old path
	if strings.Contains(oldRelPath, "..") {
		return "", fmt.Errorf("invalid old path: contains '..'")
	}

	oldFullPath := filepath.Join(fs.basePath, oldRelPath)

	// Check if source file exists
	if _, err := os.Stat(oldFullPath); err != nil {
		return "", fmt.Errorf("source file not found: %w", err)
	}

	// Create destination mailbox directory
	sanitized := sanitizeMailboxName(newMailbox)
	newMailboxPath := filepath.Join(fs.basePath, sanitized)
	if err := os.MkdirAll(newMailboxPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create destination mailbox: %w", err)
	}

	// New file path
	newFullPath := filepath.Join(newMailboxPath, fmt.Sprintf("%d.eml", newMailID))

	// Move file
	if err := os.Rename(oldFullPath, newFullPath); err != nil {
		return "", fmt.Errorf("failed to move file: %w", err)
	}

	// Return new relative path
	newRelPath, err := filepath.Rel(fs.basePath, newFullPath)
	if err != nil {
		return "", fmt.Errorf("failed to get relative path: %w", err)
	}

	return newRelPath, nil
}

// GetTotalSize returns the total size of all stored mail files in bytes
func (fs *FileStore) GetTotalSize() (int64, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	var totalSize int64

	err := filepath.Walk(fs.basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".eml") {
			totalSize += info.Size()
		}
		return nil
	})

	if err != nil {
		return 0, fmt.Errorf("failed to calculate total size: %w", err)
	}

	return totalSize, nil
}

// CleanupOrphanedFiles removes mail files that are not referenced in the database
// This requires access to the storage layer to query existing mail records
// Returns the number of files deleted and total bytes freed
func (fs *FileStore) CleanupOrphanedFiles(validPaths map[string]bool) (int, int64, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	deletedCount := 0
	var deletedSize int64

	err := filepath.Walk(fs.basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories and non-.eml files
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".eml") {
			return nil
		}

		// Get relative path
		relPath, err := filepath.Rel(fs.basePath, path)
		if err != nil {
			return err
		}

		// Check if this file is in the valid paths map
		if !validPaths[relPath] {
			// Orphaned file - delete it
			size := info.Size()
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("failed to delete orphaned file %s: %w", relPath, err)
			}
			deletedCount++
			deletedSize += size
		}

		return nil
	})

	if err != nil {
		return deletedCount, deletedSize, fmt.Errorf("cleanup failed: %w", err)
	}

	return deletedCount, deletedSize, nil
}

// GetMailboxSize returns the total size of files in a specific mailbox
func (fs *FileStore) GetMailboxSize(mailbox string) (int64, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	sanitized := sanitizeMailboxName(mailbox)
	mailboxPath := filepath.Join(fs.basePath, sanitized)

	var totalSize int64

	err := filepath.Walk(mailboxPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // Mailbox doesn't exist, return 0
			}
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".eml") {
			totalSize += info.Size()
		}
		return nil
	})

	if err != nil {
		return 0, fmt.Errorf("failed to calculate mailbox size: %w", err)
	}

	return totalSize, nil
}

// BasePath returns the base path of the file store
func (fs *FileStore) BasePath() string {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.basePath
}

// sanitizeMailboxName removes potentially dangerous characters from mailbox names
func sanitizeMailboxName(mailbox string) string {
	// Replace path separators and other dangerous characters
	sanitized := strings.ReplaceAll(mailbox, "/", "_")
	sanitized = strings.ReplaceAll(sanitized, "\\", "_")
	sanitized = strings.ReplaceAll(sanitized, "..", "_")
	sanitized = strings.ReplaceAll(sanitized, ":", "_")
	sanitized = strings.TrimSpace(sanitized)

	if sanitized == "" {
		sanitized = "default"
	}

	return sanitized
}
