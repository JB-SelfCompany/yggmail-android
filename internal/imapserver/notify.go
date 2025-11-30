/*
 *  Copyright (c) 2021 Neil Alexander
 *
 *  This Source Code Form is subject to the terms of the Mozilla Public
 *  License, v. 2.0. If a copy of the MPL was not distributed with this
 *  file, You can obtain one at http://mozilla.org/MPL/2.0/.
 */

package imapserver

import (
	"fmt"
	"log"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/server"
)

type IMAPNotifyHandler struct {
	imap.Command
}

func (h *IMAPNotifyHandler) Handle(conn server.Conn) error {
	// TODO: Support setting NOTIFY subscriptions or not
	return nil
}

type IMAPNotify struct {
	server *server.Server
	log    *log.Logger
}

func (ext *IMAPNotify) Capabilities(c server.Conn) []string {
	if c.Context().State&imap.AuthenticatedState != 0 {
		return []string{"NOTIFY"}
	}
	return nil
}

func (ext *IMAPNotify) Command(name string) server.HandlerFactory {
	if name != "NOTIFY" {
		return nil
	}
	return func() server.Handler {
		return &IMAPNotifyHandler{}
	}
}

func (ext *IMAPNotify) NotifyNew(id, count int) error {
	// Skip logging for heartbeat messages (id == -1)
	isHeartbeat := id == -1

	if !isHeartbeat {
		ext.log.Printf("NotifyNew called: mailID=%d, totalCount=%d", id, count)
	}

	ext.server.ForEachConn(func(c server.Conn) {
		// Check if client is in INBOX and send EXISTS/RECENT untagged responses
		if mailbox := c.Context().Mailbox; mailbox != nil && mailbox.Name() == "INBOX" {
			if !isHeartbeat {
				ext.log.Printf("Sending untagged EXISTS %d to IDLE client in INBOX", count)
			}

			// Send untagged EXISTS response (total message count)
			_ = c.WriteResp(&imap.StatusResp{
				Type: imap.StatusRespType(fmt.Sprintf("%d EXISTS", count)),
			})

			// Only send RECENT for actual new mail, not heartbeats
			if !isHeartbeat {
				// Send untagged RECENT response
				_ = c.WriteResp(&imap.StatusResp{
					Type: imap.StatusRespType(fmt.Sprintf("%d RECENT", 1)),
				})
			}
		} else {
			// For clients not currently in INBOX, send STATUS notification
			if !isHeartbeat {
				ext.log.Printf("Sending STATUS notification to client not in INBOX")
			}
			_ = c.WriteResp(&imap.StatusResp{
				Type: imap.StatusRespType(fmt.Sprintf("STATUS INBOX (UIDNEXT %d MESSAGES %d)", id+1, count)),
			})
		}
	})

	if !isHeartbeat {
		ext.log.Println("NotifyNew completed successfully")
	}
	return nil
}

func NewIMAPNotify(s *server.Server, log *log.Logger) *IMAPNotify {
	return &IMAPNotify{
		server: s,
		log:    log,
	}
}
