/*
 *  Copyright (c) 2021 Neil Alexander
 *
 *  This Source Code Form is subject to the terms of the Mozilla Public
 *  License, v. 2.0. If a copy of the MPL was not distributed with this
 *  file, You can obtain one at http://mozilla.org/MPL/2.0/.
 */

package types

import "time"

type Mail struct {
	Mailbox  string
	ID       int
	Mail     []byte  // NULL for large messages stored in files
	MailFile string  // Relative path to .eml file (for large messages)
	Size     int64   // Size in bytes (for both BLOB and file storage)
	Date     time.Time
	Seen     bool
	Answered bool
	Flagged  bool
	Deleted  bool
}

type QueuedMail struct {
	ID   int
	From string
	Rcpt string
}

// Constants for large message handling
const (
	SmallMessageThreshold = 10 * 1024 * 1024   // 10 MB - threshold for storing in DB vs file
	LargeMessageThreshold = 500 * 1024 * 1024  // 500 MB - maximum message size
	ChunkSize             = 128 * 1024         // 128 KB - chunk size for streaming I/O
)
