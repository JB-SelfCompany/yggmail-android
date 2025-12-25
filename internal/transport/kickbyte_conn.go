/*
 *  Copyright (c) 2021 Neil Alexander
 *
 *  This Source Code Form is subject to the terms of the Mozilla Public
 *  License, v. 2.0. If a copy of the MPL was not distributed with this
 *  file, You can obtain one at http://mozilla.org/MPL/2.0/.
 */

package transport

import (
	"io"
	"net"
	"sync"
)

// kickByteConn wraps a net.Conn and consumes the first byte on the first Read().
// This is needed because QUIC streams require a "kick" byte to trigger the server
// to send the SMTP greeting. We consume this kick byte before passing data to SMTP.
type kickByteConn struct {
	net.Conn
	once sync.Once
}

// newKickByteConn creates a new connection wrapper that will consume the kick byte.
func newKickByteConn(conn net.Conn) net.Conn {
	return &kickByteConn{Conn: conn}
}

// Read consumes the kick byte on first read, then passes through all subsequent reads.
func (c *kickByteConn) Read(b []byte) (n int, err error) {
	var consumed bool
	c.once.Do(func() {
		// Read and discard exactly one byte (the kick byte)
		kickBuf := make([]byte, 1)
		_, err = io.ReadFull(c.Conn, kickBuf)
		if err != nil {
			consumed = false
			return
		}
		consumed = true
	})

	// If we failed to consume the kick byte, return the error
	if !consumed && err != nil {
		return 0, err
	}

	// Pass through to underlying connection
	return c.Conn.Read(b)
}
