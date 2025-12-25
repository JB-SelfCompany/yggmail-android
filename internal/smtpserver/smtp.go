/*
 *  Copyright (c) 2021 Neil Alexander
 *
 *  This Source Code Form is subject to the terms of the Mozilla Public
 *  License, v. 2.0. If a copy of the MPL was not distributed with this
 *  file, You can obtain one at http://mozilla.org/MPL/2.0/.
 */

package smtpserver

import (
	"github.com/emersion/go-smtp"
	"github.com/JB-SelfCompany/yggmail/internal/imapserver"
)

type SMTPServer struct {
	server  *smtp.Server
	backend smtp.Backend
	notify  *imapserver.IMAPNotify
}

func NewSMTPServer(backend smtp.Backend, notify *imapserver.IMAPNotify) *SMTPServer {
	srv := smtp.NewServer(backend)
	// Allow large messages up to 500 MB
	srv.MaxMessageBytes = 500 * 1024 * 1024
	s := &SMTPServer{
		server:  srv,
		backend: backend,
		notify:  notify,
	}
	return s
}
