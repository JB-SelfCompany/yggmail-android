/*
 *  Copyright (c) 2021 Neil Alexander
 *
 *  This Source Code Form is subject to the terms of the Mozilla Public
 *  License, v. 2.0. If a copy of the MPL was not distributed with this
 *  file, You can obtain one at http://mozilla.org/MPL/2.0/.
 */

package smtpsender

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/neilalexander/yggmail/internal/config"
	"github.com/neilalexander/yggmail/internal/storage"
	"github.com/neilalexander/yggmail/internal/transport"
	"github.com/neilalexander/yggmail/internal/utils"
	"go.uber.org/atomic"
)

const (
	// Battery-optimized retry strategy with long delays for mobile networks
	// Retries at: 60, 180, 420, 900, 1860, 3720 seconds from start (then capped at 3600)
	MIN_BACKOFF_SECONDS        = 60    // Start at 60 seconds (reduced from 30 for battery)
	MAX_BACKOFF_SECONDS        = 3600  // Cap at 60 minutes (increased from 32 for battery)
	BACKOFF_MULTIPLIER         = 2.0   // Exponential growth factor
	UNDELIVERABLE_TIMEOUT_SECS = 604800 // 7 days - report as undeliverable after this time

	// Adaptive queue manager intervals for battery optimization
	MANAGER_INTERVAL_ACTIVE = 60  // 1 minute when there are queued messages
	MANAGER_INTERVAL_IDLE   = 300 // 5 minutes when queue is empty
)

type Queues struct {
	Config    *config.Config
	Log       *log.Logger
	Transport transport.Transport
	Storage   storage.Storage
	queues    sync.Map // servername -> *Queue
	triggerCh chan struct{} // Channel to trigger immediate queue processing
}

func NewQueues(config *config.Config, log *log.Logger, transport transport.Transport, storage storage.Storage) *Queues {
	qs := &Queues{
		Config:    config,
		Log:       log,
		Transport: transport,
		Storage:   storage,
		triggerCh: make(chan struct{}, 1), // Buffered channel to avoid blocking
	}
	time.AfterFunc(time.Second*5, qs.manager)
	return qs
}

func (qs *Queues) manager() {
	destinations, err := qs.Storage.QueueListDestinations()
	if err != nil {
		return
	}
	for _, destination := range destinations {
		_, _ = qs.queueFor(destination)
	}

	// Adaptive interval based on queue status for battery optimization
	// Active interval (60s) when messages are queued, idle interval (5min) when empty
	// This reduces unnecessary wakeups from 2,880/day to 288/day when idle
	interval := MANAGER_INTERVAL_IDLE
	if len(destinations) > 0 {
		interval = MANAGER_INTERVAL_ACTIVE
	}

	time.AfterFunc(time.Second*time.Duration(interval), qs.manager)
}

func (qs *Queues) QueueFor(from string, rcpts []string, content []byte) error {
	pid, err := qs.Storage.MailCreate("Outbox", content)
	if err != nil {
		return fmt.Errorf("q.queues.Storage.MailCreate: %w", err)
	}

	for _, rcpt := range rcpts {
		addr, err := mail.ParseAddress(rcpt)
		if err != nil {
			return fmt.Errorf("mail.ParseAddress: %w", err)
		}
		pk, err := utils.ParseAddress(addr.Address)
		if err != nil {
			return fmt.Errorf("parseAddress: %w", err)
		}
		host := hex.EncodeToString(pk)

		if err := qs.Storage.QueueInsertDestinationForID(host, pid, from, rcpt); err != nil {
			return fmt.Errorf("qs.Storage.QueueInsertDestinationForID: %w", err)
		}

		_, _ = qs.queueFor(host)
	}

	// Trigger immediate queue processing for faster delivery
	select {
	case qs.triggerCh <- struct{}{}:
		// Successfully triggered immediate processing
	default:
		// Channel already has a trigger pending, skip
	}

	return nil
}

func (qs *Queues) queueFor(server string) (*Queue, error) {
	v, _ := qs.queues.LoadOrStore(server, &Queue{
		queues:      qs,
		destination: server,
	})
	q, ok := v.(*Queue)
	if !ok {
		return nil, fmt.Errorf("type assertion error")
	}
	if q.running.CompareAndSwap(false, true) {
		go q.run()
	}
	return q, nil
}

// ResetRetryCounters clears all retry counters and triggers immediate queue processing
// This should be called when network conditions change (WiFi <-> Mobile) or peer connections change
func (qs *Queues) ResetRetryCounters() {
	qs.Log.Println("[Battery Optimization] Resetting all retry counters due to network/peer change")
	qs.queues.Range(func(key, value interface{}) bool {
		if q, ok := value.(*Queue); ok {
			q.retryCount = 0
			q.lastAttempt = time.Time{} // Reset last attempt to allow immediate retry
			q.permanentFailure = false
		}
		return true
	})

	// Trigger immediate queue processing
	select {
	case qs.triggerCh <- struct{}{}:
		qs.Log.Println("[Battery Optimization] Triggered immediate queue processing after reset")
	default:
		// Channel already has a trigger pending
	}
}

type Queue struct {
	queues           *Queues
	destination      string
	running          atomic.Bool
	retryCount       int
	lastAttempt      time.Time
	permanentFailure bool // Mark as permanently failed after max retries
}

func (q *Queue) run() {
	defer q.running.Store(false)
	defer q.queues.Storage.MailExpunge("Outbox") // nolint:errcheck

	// Battery-optimized exponential backoff with jitter
	if !q.lastAttempt.IsZero() && q.retryCount > 0 {
		backoffSeconds := calculateBackoff(q.retryCount)
		waitTime := time.Duration(backoffSeconds) * time.Second
		elapsed := time.Since(q.lastAttempt)
		if elapsed < waitTime {
			remaining := waitTime - elapsed
			q.queues.Log.Printf("[Battery Optimization] Waiting %v before retry to %s (attempt %d)\n",
				remaining, q.destination, q.retryCount+1)
			time.Sleep(remaining)
		}
	}
	q.lastAttempt = time.Now()

	refs, err := q.queues.Storage.QueueMailIDsForDestination(q.destination)
	if err != nil {
		q.queues.Log.Println("Error with queue:", err)
		q.retryCount++
		return
	}

	for _, ref := range refs {
		_, mail, err := q.queues.Storage.MailSelect("Outbox", ref.ID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				q.queues.Storage.QueueDeleteDestinationForID("Outbox", ref.ID)
			} else {
				q.queues.Log.Println("Failed to get mail", ref.ID, "due to error:", err)
			}
			continue
		}

		// Check if message has been undeliverable for too long
		messageAge := time.Since(mail.Date)
		if messageAge > time.Duration(UNDELIVERABLE_TIMEOUT_SECS)*time.Second {
			q.queues.Log.Printf("[Battery Optimization] Message %d to %s is undeliverable (age: %v, max: %v) - marking as failed\n",
				ref.ID, q.destination, messageAge, time.Duration(UNDELIVERABLE_TIMEOUT_SECS)*time.Second)

			// Remove from queue
			q.queues.Storage.QueueDeleteDestinationForID(q.destination, ref.ID)
			if remaining, err := q.queues.Storage.QueueSelectIsMessagePendingSend("Outbox", ref.ID); err == nil && !remaining {
				q.queues.Storage.MailDelete("Outbox", ref.ID)
			}
			// TODO: Move to "Failed" mailbox or notify user
			continue
		}

		// Check if this is a self-send (loopback)
		myAddress := hex.EncodeToString(q.queues.Config.PublicKey)
		isSelfSend := q.destination == myAddress

		if isSelfSend {
			// Local delivery - avoid network connection race condition
			q.queues.Log.Println("Local delivery from", ref.From, "to", q.destination, "(self-send)")

			// Directly store the mail in the Inbox
			if _, err := q.queues.Storage.MailCreate("INBOX", mail.Mail); err != nil {
				q.retryCount++
				q.queues.Log.Printf("Failed local delivery to %s (retry count: %d): %v\n", q.destination, q.retryCount, err)
				continue
			}

			// Remove from queue
			if err := q.queues.Storage.QueueDeleteDestinationForID(q.destination, ref.ID); err != nil {
				q.queues.Log.Printf("Failed to remove from queue: %v\n", err)
			}

			if remaining, err := q.queues.Storage.QueueSelectIsMessagePendingSend("Outbox", ref.ID); err != nil {
				q.queues.Log.Printf("Failed to check pending: %v\n", err)
			} else if !remaining {
				q.queues.Storage.MailDelete("Outbox", ref.ID)
			}

			q.retryCount = 0
			q.queues.Log.Println("Local delivery successful from", ref.From, "to", q.destination)
			continue
		}

		// Remote delivery via QUIC
		q.queues.Log.Println("Sending mail from", ref.From, "to", q.destination)

		if err := func() error {
			conn, err := q.queues.Transport.Dial(q.destination)
			if err != nil {
				return fmt.Errorf("q.queues.Transport.Dial: %w", err)
			}
			defer conn.Close()

			client, err := smtp.NewClient(conn, q.destination)
			if err != nil {
				return fmt.Errorf("smtp.NewClient: %w", err)
			}
			defer client.Close()

			if err := client.Hello(hex.EncodeToString(q.queues.Config.PublicKey)); err != nil {
				q.queues.Log.Println("Remote server", q.destination, "did not accept HELLO:", err)
				return fmt.Errorf("client.Hello: %w", err)
			}

			if err := client.Mail(ref.From, nil); err != nil {
				q.queues.Log.Println("Remote server", q.destination, "did not accept MAIL:", err)
				return fmt.Errorf("client.Mail: %w", err)
			}

			if err := client.Rcpt(ref.Rcpt); err != nil {
				q.queues.Log.Println("Remote server", q.destination, "did not accept RCPT:", err)
				return fmt.Errorf("client.Rcpt: %w", err)
			}

			writer, err := client.Data()
			if err != nil {
				return fmt.Errorf("client.Data: %w", err)
			}
			defer writer.Close()

			if _, err := writer.Write(mail.Mail); err != nil {
				return fmt.Errorf("writer.Write: %w", err)
			}

			if err := q.queues.Storage.QueueDeleteDestinationForID(q.destination, ref.ID); err != nil {
				return fmt.Errorf("q.queues.Storage.QueueDeleteDestinationForID: %w", err)
			}

			if remaining, err := q.queues.Storage.QueueSelectIsMessagePendingSend("Outbox", ref.ID); err != nil {
				return fmt.Errorf("q.queues.Storage.QueueSelectIsMessagePendingSend: %w", err)
			} else if !remaining {
				return q.queues.Storage.MailDelete("Outbox", ref.ID)
			}

			return nil
		}(); err != nil {
			// Classify error type for intelligent retry
			if isNetworkError(err) {
				q.retryCount++
				q.queues.Log.Printf("[Battery Optimization] Network error sending to %s (retry %d): %v\n",
					q.destination, q.retryCount, err)
				// Will retry on next manager run with exponential backoff
			} else if isPermanentError(err) {
				q.permanentFailure = true
				q.queues.Log.Printf("[Battery Optimization] Permanent error sending to %s - stopping retries: %v\n",
					q.destination, err)
				// Delete from queue to prevent retries
				q.queues.Storage.QueueDeleteDestinationForID(q.destination, ref.ID)
				if remaining, err := q.queues.Storage.QueueSelectIsMessagePendingSend("Outbox", ref.ID); err == nil && !remaining {
					q.queues.Storage.MailDelete("Outbox", ref.ID)
				}
				// TODO: Notify user of permanent failure
				return
			} else {
				// Unknown error - treat as temporary
				q.retryCount++
				q.queues.Log.Printf("Failed to send to %s (retry %d): %v\n",
					q.destination, q.retryCount, err)
				// Will retry on next manager run with exponential backoff
			}
		} else {
			q.retryCount = 0 // Reset retry count on success
			q.permanentFailure = false
			q.queues.Log.Println("Sent mail from", ref.From, "to", q.destination)
		}
	}
}

// calculateBackoff calculates exponential backoff with jitter
func calculateBackoff(retryCount int) int {
	// Exponential backoff: 30, 60, 120, 240, 480, 960, 1920, 1920...
	backoff := float64(MIN_BACKOFF_SECONDS) * math.Pow(BACKOFF_MULTIPLIER, float64(retryCount-1))

	// Cap at maximum
	if backoff > float64(MAX_BACKOFF_SECONDS) {
		backoff = float64(MAX_BACKOFF_SECONDS)
	}

	// Add jitter (±20%) to prevent thundering herd
	jitter := rand.Float64()*0.4 - 0.2 // -20% to +20%
	backoff = backoff * (1.0 + jitter)

	return int(backoff)
}

// isNetworkError checks if error is network-related (temporary)
func isNetworkError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "context deadline exceeded") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "network is unreachable") ||
		strings.Contains(errStr, "no route to host") ||
		strings.Contains(errStr, "Transport.Dial") ||
		strings.Contains(errStr, "i/o timeout")
}

// isPermanentError checks if error is permanent (don't retry)
func isPermanentError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "invalid address") ||
		strings.Contains(errStr, "mailbox not found") ||
		strings.Contains(errStr, "permanent failure") ||
		strings.Contains(errStr, "recipient rejected")
}
