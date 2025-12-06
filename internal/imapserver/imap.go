/*
 *  Copyright (c) 2021 Neil Alexander
 *
 *  This Source Code Form is subject to the terms of the Mozilla Public
 *  License, v. 2.0. If a copy of the MPL was not distributed with this
 *  file, You can obtain one at http://mozilla.org/MPL/2.0/.
 */

package imapserver

import (
	"log"
	"time"

	idle "github.com/emersion/go-imap-idle"
	move "github.com/emersion/go-imap-move"
	"github.com/emersion/go-imap/server"
	"github.com/emersion/go-sasl"
)

type IMAPServer struct {
	server  *server.Server
	backend *Backend
	notify  *IMAPNotify
	done    chan struct{}
	log     *log.Logger
}

func NewIMAPServer(backend *Backend, addr string, insecure bool) (*IMAPServer, *IMAPNotify, error) {
	s := &IMAPServer{
		server:  server.New(backend),
		backend: backend,
		done:    make(chan struct{}),
		log:     backend.Log,
	}
	s.notify = NewIMAPNotify(s.server, backend.Log)
	s.server.Addr = addr
	s.server.AllowInsecureAuth = insecure
	//s.server.Debug = os.Stdout
	s.server.Enable(idle.NewExtension())
	s.server.Enable(move.NewExtension())
	// s.server.Enable(s.notify)
	s.server.EnableAuth(sasl.Login, func(conn server.Conn) sasl.Server {
		return sasl.NewLoginServer(func(username, password string) error {
			_, err := s.backend.Login(nil, username, password)
			return err
		})
	})
	go func() {
		defer close(s.done)
		if err := s.server.ListenAndServe(); err != nil {
			// Don't use log.Fatal() during shutdown - it causes panic
			// Only log real errors, not expected "use of closed network connection"
			if s.server != nil && err.Error() != "use of closed network connection" {
				s.log.Printf("IMAP server error: %v\n", err)
			}
		}
	}()
	return s, s.notify, nil
}

// Close closes the IMAP server and waits for goroutine to exit
func (s *IMAPServer) Close() error {
	if s.server != nil {
		if err := s.server.Close(); err != nil {
			return err
		}
		// Wait for server goroutine to exit (with timeout)
		select {
		case <-s.done:
			// Goroutine exited cleanly
		case <-time.After(2 * time.Second):
			// Timeout - continue anyway
			s.log.Println("Warning: IMAP server goroutine did not exit within timeout")
		}
	}
	return nil
}