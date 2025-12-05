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
	"net/mail"
	"sync"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/neilalexander/yggmail/internal/config"
	"github.com/neilalexander/yggmail/internal/storage"
	"github.com/neilalexander/yggmail/internal/transport"
	"github.com/neilalexander/yggmail/internal/utils"
	"go.uber.org/atomic"
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
	// Reduced retry interval for faster delivery attempts (mobile-friendly)
	time.AfterFunc(time.Second*30, qs.manager)
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

type Queue struct {
	queues      *Queues
	destination string
	running     atomic.Bool
	retryCount  int
	lastAttempt time.Time
}

func (q *Queue) run() {
	defer q.running.Store(false)
	defer q.queues.Storage.MailExpunge("Outbox") // nolint:errcheck

	// Implement exponential backoff for mobile network stability
	// Reduced delays for faster retry: 1s, 2s, 4s, 8s, 15s (max)
	if !q.lastAttempt.IsZero() && q.retryCount > 0 {
		var backoffSeconds int
		if q.retryCount <= 4 {
			backoffSeconds = 1 << uint(q.retryCount-1) // 1, 2, 4, 8
		} else {
			backoffSeconds = 15 // Cap at 15 seconds instead of 60
		}
		waitTime := time.Duration(backoffSeconds) * time.Second
		elapsed := time.Since(q.lastAttempt)
		if elapsed < waitTime {
			remaining := waitTime - elapsed
			q.queues.Log.Printf("Waiting %v before retry to %s (attempt %d)\n", remaining, q.destination, q.retryCount+1)
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
			q.retryCount++
			q.queues.Log.Printf("Failed to send to %s (retry count: %d): %v\n", q.destination, q.retryCount, err)
			// TODO: Send a mail to the inbox on the first instance?
		} else {
			q.retryCount = 0 // Reset retry count on success
			q.queues.Log.Println("Sent mail from", ref.From, "to", q.destination)
		}
	}
}
