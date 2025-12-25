/*
 *  Copyright (c) 2021 Neil Alexander
 *
 *  This Source Code Form is subject to the terms of the Mozilla Public
 *  License, v. 2.0. If a copy of the MPL was not distributed with this
 *  file, You can obtain one at http://mozilla.org/MPL/2.0/.
 */

package smtpserver

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-smtp"
	"github.com/JB-SelfCompany/yggmail/internal/utils"
)

type SessionLocal struct {
	backend *Backend
	state   *smtp.ConnectionState
	from    string
	rcpt    []string
}

func (s *SessionLocal) Mail(from string, opts smtp.MailOptions) error {
	s.rcpt = s.rcpt[:0]

	pk, err := utils.ParseAddress(from)
	if err != nil {
		return fmt.Errorf("parseAddress: %w", err)
	}

	if !pk.Equal(s.backend.Config.PublicKey) {
		return fmt.Errorf("not allowed to send outgoing mail as %s", from)
	}

	s.from = from
	return nil
}

func (s *SessionLocal) Rcpt(to string) error {
	s.rcpt = append(s.rcpt, to)
	return nil
}

func (s *SessionLocal) Data(r io.Reader) error {
	// Simply read all data and queue it
	// Don't do complex MultiReader - just read, add headers, and queue
	var b bytes.Buffer
	written, err := io.Copy(&b, r)
	if err != nil {
		return fmt.Errorf("failed to read message data: %w", err)
	}

	fmt.Printf("[SessionLocal] Data() received: %d bytes (%.2f MB)\n", written, float64(written)/(1024*1024))

	// Parse message to add headers
	m, err := message.Read(bytes.NewReader(b.Bytes()))
	if err != nil {
		return fmt.Errorf("message.Read: %w", err)
	}

	// Log Content-Type and Content-Transfer-Encoding to understand message structure
	contentType := m.Header.Get("Content-Type")
	encoding := m.Header.Get("Content-Transfer-Encoding")
	fmt.Printf("[SessionLocal] Content-Type: %s, Encoding: %s\n", contentType, encoding)

	// Add Yggmail-specific headers
	remoteAddr := "unknown"
	if s.state != nil && s.state.RemoteAddr != nil {
		remoteAddr = s.state.RemoteAddr.String()
	}
	m.Header.Add(
		"Received", fmt.Sprintf("from %s by Yggmail %s; %s",
			remoteAddr,
			hex.EncodeToString(s.backend.Config.PublicKey),
			time.Now().String(),
		),
	)
	if !m.Header.Has("Date") {
		m.Header.Add(
			"Date", time.Now().UTC().Format(time.RFC822),
		)
	}

	// Rebuild message with new headers
	var finalData bytes.Buffer
	fields := m.Header.Fields()
	for fields.Next() {
		finalData.WriteString(fields.Key())
		finalData.WriteString(": ")
		finalData.WriteString(fields.Value())
		finalData.WriteString("\r\n")
	}
	finalData.WriteString("\r\n")
	if _, err := io.Copy(&finalData, m.Body); err != nil {
		return fmt.Errorf("failed to copy body: %w", err)
	}

	fmt.Printf("[SessionLocal] Final message size: %d bytes (%.2f MB)\n", finalData.Len(), float64(finalData.Len())/(1024*1024))

	// Queue the message using QueueFor (data already in memory)
	if err := s.backend.Queues.QueueFor(s.from, s.rcpt, finalData.Bytes()); err != nil {
		return fmt.Errorf("s.backend.Queues.QueueFor: %w", err)
	}
	s.backend.Log.Printf("Queued mail for %v (Size: %.2f MB)", s.rcpt, float64(finalData.Len())/(1024*1024))

	return nil
}

func (s *SessionLocal) Reset() {
	s.rcpt = s.rcpt[:0]
	s.from = ""
}

func (s *SessionLocal) Logout() error {
	return nil
}
