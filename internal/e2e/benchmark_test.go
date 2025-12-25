package e2e

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/JB-SelfCompany/yggmail/internal/logging"
	"github.com/JB-SelfCompany/yggmail/internal/storage/filestore"
	"github.com/JB-SelfCompany/yggmail/internal/storage/sqlite3"
)

// setupBenchNode creates a test node for benchmarking
func setupBenchNode(b *testing.B) (*TestNode, func()) {
	tempDir, err := os.MkdirTemp("", "yggmail-bench-*")
	if err != nil {
		b.Fatalf("Failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(tempDir, "bench.db")
	fileStorePath := filepath.Join(tempDir, "maildata")

	storage, err := sqlite3.NewSQLite3StorageStorage(dbPath)
	if err != nil {
		os.RemoveAll(tempDir)
		b.Fatalf("Failed to create storage: %v", err)
	}

	fs, err := filestore.NewFileStore(fileStorePath)
	if err != nil {
		storage.Close()
		os.RemoveAll(tempDir)
		b.Fatalf("Failed to create FileStore: %v", err)
	}

	logger := logging.NewLargeMailLogger()

	if err := storage.MailboxCreate("INBOX"); err != nil {
		storage.Close()
		os.RemoveAll(tempDir)
		b.Fatalf("Failed to create INBOX: %v", err)
	}

	node := &TestNode{
		Name:            "bench",
		TempDir:         tempDir,
		Storage:         storage,
		FileStore:       fs,
		LargeMailLogger: logger,
	}

	cleanup := func() {
		storage.Close()
		os.RemoveAll(tempDir)
	}

	return node, cleanup
}

// BenchmarkBufferSizes tests different buffer sizes for streaming I/O
func BenchmarkBufferSizes(b *testing.B) {
	bufferSizes := []int{
		32 * 1024,   // 32 KB
		64 * 1024,   // 64 KB
		128 * 1024,  // 128 KB (current default)
		256 * 1024,  // 256 KB
		512 * 1024,  // 512 KB
		1024 * 1024, // 1 MB
	}

	messageSize := 50 * 1024 * 1024 // 50 MB test message

	for _, bufSize := range bufferSizes {
		b.Run(fmt.Sprintf("BufferSize_%dKB", bufSize/1024), func(b *testing.B) {
			node, cleanup := setupBenchNode(b)
			defer cleanup()

			// Generate test data once
			messageData := make([]byte, messageSize)
			rand.Read(messageData)

			b.ResetTimer()
			b.SetBytes(int64(messageSize))

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				reader := &bufferedReader{
					reader:     bytes.NewReader(messageData),
					bufferSize: bufSize,
				}
				b.StartTimer()

				mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
				if err != nil {
					b.Fatalf("Failed to create message: %v", err)
				}

				b.StopTimer()
				// Clean up for next iteration
				node.Storage.MailDelete("INBOX", mailID)
				b.StartTimer()
			}

			b.ReportMetric(float64(messageSize)/(1024*1024), "MB/op")
		})
	}
}

// bufferedReader wraps an io.Reader with a specific buffer size
type bufferedReader struct {
	reader     io.Reader
	bufferSize int
	buffer     []byte
}

func (br *bufferedReader) Read(p []byte) (n int, err error) {
	if br.buffer == nil {
		br.buffer = make([]byte, br.bufferSize)
	}

	// Read in chunks of bufferSize
	readSize := len(p)
	if readSize > br.bufferSize {
		readSize = br.bufferSize
	}

	return br.reader.Read(p[:readSize])
}

// BenchmarkSmallVsLargeMessage compares performance of small vs large messages
func BenchmarkSmallVsLargeMessage(b *testing.B) {
	testCases := []struct {
		name string
		size int
	}{
		{"1MB_BLOB", 1 * 1024 * 1024},
		{"5MB_BLOB", 5 * 1024 * 1024},
		{"9MB_BLOB", 9 * 1024 * 1024},
		{"10MB_File", 10 * 1024 * 1024},
		{"20MB_File", 20 * 1024 * 1024},
		{"50MB_File", 50 * 1024 * 1024},
		{"100MB_File", 100 * 1024 * 1024},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			node, cleanup := setupBenchNode(b)
			defer cleanup()

			messageData := make([]byte, tc.size)
			rand.Read(messageData)

			b.ResetTimer()
			b.SetBytes(int64(tc.size))

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				reader := bytes.NewReader(messageData)
				b.StartTimer()

				mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
				if err != nil {
					b.Fatalf("Failed to create message: %v", err)
				}

				b.StopTimer()
				node.Storage.MailDelete("INBOX", mailID)
				b.StartTimer()
			}

			b.ReportMetric(float64(tc.size)/(1024*1024), "MB/op")
		})
	}
}

// BenchmarkReadPerformance tests read performance for different storage types
func BenchmarkReadPerformance(b *testing.B) {
	testCases := []struct {
		name string
		size int
	}{
		{"Read_5MB_BLOB", 5 * 1024 * 1024},
		{"Read_20MB_File", 20 * 1024 * 1024},
		{"Read_50MB_File", 50 * 1024 * 1024},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			node, cleanup := setupBenchNode(b)
			defer cleanup()

			// Create message once
			messageData := make([]byte, tc.size)
			rand.Read(messageData)
			reader := bytes.NewReader(messageData)

			mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
			if err != nil {
				b.Fatalf("Failed to create message: %v", err)
			}

			b.ResetTimer()
			b.SetBytes(int64(tc.size))

			for i := 0; i < b.N; i++ {
				_, mail, err := node.Storage.MailSelect("INBOX", mailID)
				if err != nil {
					b.Fatalf("Failed to select message: %v", err)
				}

				// Read the entire message
				var mailReader io.Reader
				if mail.MailFile != "" {
					file, err := node.FileStore.ReadMail(mail.MailFile)
					if err != nil {
						b.Fatalf("Failed to open file: %v", err)
					}
					mailReader = file
					defer file.Close()
				} else {
					mailReader = bytes.NewReader(mail.Mail)
				}

				// Read all data
				written, err := io.Copy(io.Discard, mailReader)
				if err != nil {
					b.Fatalf("Failed to read message: %v", err)
				}

				if written != mail.Size {
					b.Fatalf("Size mismatch: expected %d, got %d", mail.Size, written)
				}
			}

			b.ReportMetric(float64(tc.size)/(1024*1024), "MB/op")
		})
	}
}

// BenchmarkParallelWrites tests concurrent write performance
func BenchmarkParallelWrites(b *testing.B) {
	parallelCounts := []int{1, 2, 4, 8}
	messageSize := 20 * 1024 * 1024 // 20 MB

	for _, parallelCount := range parallelCounts {
		b.Run(fmt.Sprintf("Parallel_%d", parallelCount), func(b *testing.B) {
			node, cleanup := setupBenchNode(b)
			defer cleanup()

			messageData := make([]byte, messageSize)
			rand.Read(messageData)

			b.ResetTimer()
			b.SetBytes(int64(messageSize * parallelCount))

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					reader := bytes.NewReader(messageData)
					mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
					if err != nil {
						b.Fatalf("Failed to create message: %v", err)
					}

					// Note: In real scenario, cleanup would be async
					_ = mailID
				}
			})
		})
	}
}

// BenchmarkMigration tests migration performance
func BenchmarkMigration(b *testing.B) {
	messageCounts := []int{10, 50, 100}
	messageSize := 15 * 1024 * 1024 // 15 MB each

	for _, count := range messageCounts {
		b.Run(fmt.Sprintf("Migrate_%d_messages", count), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				node, cleanup := setupBenchNode(b)

				// Create messages in BLOB
				messageData := make([]byte, messageSize)
				rand.Read(messageData)

				for j := 0; j < count; j++ {
					_, err := node.Storage.MailCreate("INBOX", messageData)
					if err != nil {
						b.Fatalf("Failed to create message: %v", err)
					}
				}

				b.StartTimer()

				// Run migration
				if err := node.Storage.MigrateLargeMessagesToFiles(node.FileStore, 10*1024*1024); err != nil {
					b.Fatalf("Migration failed: %v", err)
				}

				// Verify all messages were migrated
				migratedCount := 0
				for j := 1; j <= count; j++ {
					_, mail, err := node.Storage.MailSelect("INBOX", j)
					if err == nil && mail.MailFile != "" {
						migratedCount++
					}
				}
				if migratedCount != count {
					b.Fatalf("Expected %d migrations, got %d", count, migratedCount)
				}

				b.StopTimer()
				cleanup()
			}

			totalSize := int64(count * messageSize)
			b.ReportMetric(float64(totalSize)/(1024*1024), "MB/op")
		})
	}
}

// BenchmarkMemoryAllocation measures memory allocations
func BenchmarkMemoryAllocation(b *testing.B) {
	node, cleanup := setupBenchNode(b)
	defer cleanup()

	messageSize := 30 * 1024 * 1024
	messageData := make([]byte, messageSize)
	rand.Read(messageData)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		reader := bytes.NewReader(messageData)
		mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
		if err != nil {
			b.Fatalf("Failed to create message: %v", err)
		}

		b.StopTimer()
		node.Storage.MailDelete("INBOX", mailID)
		b.StartTimer()
	}
}

// BenchmarkChunkedTransfer simulates realistic network transfer
func BenchmarkChunkedTransfer(b *testing.B) {
	chunkSizes := []int{
		64 * 1024,   // 64 KB
		128 * 1024,  // 128 KB (current default)
		256 * 1024,  // 256 KB
		512 * 1024,  // 512 KB
		1024 * 1024, // 1 MB
	}

	messageSize := 50 * 1024 * 1024 // 50 MB

	for _, chunkSize := range chunkSizes {
		b.Run(fmt.Sprintf("ChunkSize_%dKB", chunkSize/1024), func(b *testing.B) {
			node, cleanup := setupBenchNode(b)
			defer cleanup()

			messageData := make([]byte, messageSize)
			rand.Read(messageData)

			b.ResetTimer()
			b.SetBytes(int64(messageSize))

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				reader := &chunkedReader{
					data:      messageData,
					chunkSize: chunkSize,
					pos:       0,
				}
				b.StartTimer()

				mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
				if err != nil {
					b.Fatalf("Failed to create message: %v", err)
				}

				b.StopTimer()
				node.Storage.MailDelete("INBOX", mailID)
				b.StartTimer()
			}

			b.ReportMetric(float64(chunkSize)/1024, "ChunkKB")
		})
	}
}

// chunkedReader simulates network transfer by reading in fixed chunks
type chunkedReader struct {
	data      []byte
	chunkSize int
	pos       int
}

func (cr *chunkedReader) Read(p []byte) (n int, err error) {
	if cr.pos >= len(cr.data) {
		return 0, io.EOF
	}

	remaining := len(cr.data) - cr.pos
	readSize := cr.chunkSize
	if readSize > remaining {
		readSize = remaining
	}
	if readSize > len(p) {
		readSize = len(p)
	}

	copy(p, cr.data[cr.pos:cr.pos+readSize])
	cr.pos += readSize

	return readSize, nil
}

// BenchmarkStorageOverhead measures database overhead
func BenchmarkStorageOverhead(b *testing.B) {
	node, cleanup := setupBenchNode(b)
	defer cleanup()

	messageSize := 15 * 1024 * 1024
	messageData := make([]byte, messageSize)
	rand.Read(messageData)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		reader := bytes.NewReader(messageData)
		b.StartTimer()

		// Measure time for MailCreateFromStream
		start := b.Elapsed()
		mailID, err := node.Storage.MailCreateFromStream("INBOX", reader, node.FileStore)
		if err != nil {
			b.Fatalf("Failed to create message: %v", err)
		}
		duration := b.Elapsed() - start

		b.StopTimer()

		// Measure file size vs message size
		_, mail, err := node.Storage.MailSelect("INBOX", mailID)
		if err != nil {
			b.Fatalf("Failed to select: %v", err)
		}

		overhead := float64(mail.Size-int64(messageSize)) / float64(messageSize) * 100

		b.ReportMetric(float64(duration.Milliseconds()), "ms/op")
		b.ReportMetric(overhead, "overhead_%")

		node.Storage.MailDelete("INBOX", mailID)
		b.StartTimer()
	}
}
