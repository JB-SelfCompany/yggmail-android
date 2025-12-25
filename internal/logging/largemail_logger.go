package logging

import (
	"log"
	"sync"
	"time"
)

// OperationLog tracks a single large mail operation (send/receive)
type OperationLog struct {
	OpID      string
	MailID    int
	TotalSize int64
	Stage     string // "RECEIVE", "SEND", "STORAGE"
	StartTime time.Time
	Milestones []Milestone
	mu        sync.Mutex
}

// Milestone represents a progress checkpoint
type Milestone struct {
	Timestamp time.Time
	Stage     string
	BytesSent int64
	Message   string
}

// LargeMailLogger manages logging for large mail operations
type LargeMailLogger struct {
	operations sync.Map // map[string]*OperationLog
}

// NewLargeMailLogger creates a new logger instance
func NewLargeMailLogger() *LargeMailLogger {
	return &LargeMailLogger{}
}

// StartOperation begins tracking a new large mail operation
func (l *LargeMailLogger) StartOperation(opID string, mailID int, size int64, stage string) {
	op := &OperationLog{
		OpID:       opID,
		MailID:     mailID,
		TotalSize:  size,
		Stage:      stage,
		StartTime:  time.Now(),
		Milestones: make([]Milestone, 0),
	}

	l.operations.Store(opID, op)

	// Log start
	log.Printf("[LargeMail:%s] START %s - MailID=%d Size=%d bytes (%.2f MB)",
		opID, stage, mailID, size, float64(size)/(1024*1024))
}

// LogMilestone records a progress checkpoint with speed calculation
func (l *LargeMailLogger) LogMilestone(opID string, stage string, bytesSent int64, message string) {
	value, ok := l.operations.Load(opID)
	if !ok {
		log.Printf("[LargeMail:%s] WARNING: Operation not found for milestone", opID)
		return
	}

	op := value.(*OperationLog)
	op.mu.Lock()
	defer op.mu.Unlock()

	milestone := Milestone{
		Timestamp: time.Now(),
		Stage:     stage,
		BytesSent: bytesSent,
		Message:   message,
	}
	op.Milestones = append(op.Milestones, milestone)

	// Calculate speed
	elapsed := time.Since(op.StartTime).Seconds()
	var speed float64
	if elapsed > 0 {
		speed = float64(bytesSent) / elapsed / (1024 * 1024) // MB/s
	}

	// Calculate percentage
	var percentage float64
	if op.TotalSize > 0 {
		percentage = float64(bytesSent) / float64(op.TotalSize) * 100
	}

	log.Printf("[LargeMail:%s] %s - %.1f%% (%d/%d) Speed=%.2f MB/s - %s",
		opID, stage, percentage, bytesSent, op.TotalSize, speed, message)
}

// EndOperation finalizes an operation with success/failure status
func (l *LargeMailLogger) EndOperation(opID string, success bool, errorMsg string) {
	value, ok := l.operations.Load(opID)
	if !ok {
		log.Printf("[LargeMail:%s] WARNING: Operation not found for end", opID)
		return
	}

	op := value.(*OperationLog)
	op.mu.Lock()
	elapsed := time.Since(op.StartTime)
	op.mu.Unlock()

	var avgSpeed float64
	if elapsed.Seconds() > 0 {
		avgSpeed = float64(op.TotalSize) / elapsed.Seconds() / (1024 * 1024) // MB/s
	}

	if success {
		log.Printf("[LargeMail:%s] SUCCESS - MailID=%d Duration=%v AvgSpeed=%.2f MB/s",
			opID, op.MailID, elapsed.Round(time.Millisecond), avgSpeed)
	} else {
		log.Printf("[LargeMail:%s] FAILED - MailID=%d Duration=%v Error: %s",
			opID, op.MailID, elapsed.Round(time.Millisecond), errorMsg)
	}

	// Remove operation from tracking
	l.operations.Delete(opID)
}

// LogError logs an error during an operation
func (l *LargeMailLogger) LogError(opID string, stage string, err error) {
	log.Printf("[LargeMail:%s] ERROR in %s: %v", opID, stage, err)
}

// LogWarning logs a warning during an operation
func (l *LargeMailLogger) LogWarning(opID string, stage string, message string) {
	log.Printf("[LargeMail:%s] WARNING in %s: %s", opID, stage, message)
}

// GetActiveOperations returns the count of currently active operations
func (l *LargeMailLogger) GetActiveOperations() int {
	count := 0
	l.operations.Range(func(key, value interface{}) bool {
		count++
		return true
	})
	return count
}

// GetOperationStatus returns the current status of an operation
func (l *LargeMailLogger) GetOperationStatus(opID string) (stage string, progress float64, speed float64, found bool) {
	value, ok := l.operations.Load(opID)
	if !ok {
		return "", 0, 0, false
	}

	op := value.(*OperationLog)
	op.mu.Lock()
	defer op.mu.Unlock()

	if len(op.Milestones) == 0 {
		return op.Stage, 0, 0, true
	}

	// Get latest milestone
	latest := op.Milestones[len(op.Milestones)-1]

	// Calculate progress
	if op.TotalSize > 0 {
		progress = float64(latest.BytesSent) / float64(op.TotalSize) * 100
	}

	// Calculate current speed
	elapsed := time.Since(op.StartTime).Seconds()
	if elapsed > 0 {
		speed = float64(latest.BytesSent) / elapsed / (1024 * 1024)
	}

	return latest.Stage, progress, speed, true
}
