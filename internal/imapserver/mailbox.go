/*
 *  Copyright (c) 2021 Neil Alexander
 *
 *  This Source Code Form is subject to the terms of the Mozilla Public
 *  License, v. 2.0. If a copy of the MPL was not distributed with this
 *  file, You can obtain one at http://mozilla.org/MPL/2.0/.
 */

package imapserver

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-message/textproto"
	"github.com/JB-SelfCompany/yggmail/internal/storage/types"
)

type Mailbox struct {
	backend *Backend
	name    string
	user    *User
}

// readerWithCloser wraps an io.Reader and calls a close function when done
type readerWithCloser struct {
	reader  io.Reader
	closeFn func() error
	closed  bool
}

func (r *readerWithCloser) Read(p []byte) (n int, err error) {
	n, err = r.reader.Read(p)
	if err == io.EOF && !r.closed {
		r.closeFn()
		r.closed = true
	}
	return n, err
}

func (r *readerWithCloser) Close() error {
	if !r.closed {
		r.closed = true
		return r.closeFn()
	}
	return nil
}

func (mbox *Mailbox) getIDsFromSeqSet(uid bool, seqSet *imap.SeqSet) ([]int32, error) {
	var ids []int32
	for _, set := range seqSet.Set {
		if set.Stop == 0 {
			next, err := mbox.backend.Storage.MailNextID(mbox.name)
			if err != nil {
				return nil, fmt.Errorf("mbox.backend.Storage.MailNextID: %w", err)
			}
			set.Stop = uint32(next - 1)
		}
		for i := set.Start; i <= set.Stop; i++ {
			if !uid {
				pid, err := mbox.backend.Storage.MailIDForSeq(mbox.name, int(i))
				if err != nil {
					return nil, fmt.Errorf("mbox.backend.Storage.MailIDForSeq: %w", err)
				}
				ids = append(ids, int32(pid))
			} else {
				ids = append(ids, int32(i))
			}
		}
	}
	return ids, nil
}

func (mbox *Mailbox) Name() string {
	return mbox.name
}

func (mbox *Mailbox) Info() (*imap.MailboxInfo, error) {
	info := &imap.MailboxInfo{
		Attributes: []string{},
		Delimiter:  "/",
		Name:       mbox.name,
	}
	return info, nil
}

func (mbox *Mailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	status := imap.NewMailboxStatus(mbox.name, items)
	status.PermanentFlags = []string{
		"\\Seen", "\\Answered", "\\Flagged", "\\Deleted",
	}
	status.Flags = status.PermanentFlags

	for _, name := range items {
		switch name {
		case imap.StatusMessages:
			count, err := mbox.backend.Storage.MailCount(mbox.name)
			if err != nil {
				return nil, fmt.Errorf("mbox.backend.Storage.MailCount: %w", err)
			}
			status.Messages = uint32(count)

		case imap.StatusUidNext:
			id, err := mbox.backend.Storage.MailNextID(mbox.name)
			if err != nil {
				return nil, fmt.Errorf("mbox.backend.Storage.MailNextID: %w", err)
			}
			status.UidNext = uint32(id)

		case imap.StatusUidValidity:
			status.UidValidity = 1

		case imap.StatusRecent:
			status.Recent = 0 

		case imap.StatusUnseen:
			unseen, err := mbox.backend.Storage.MailUnseen(mbox.name)
			if err != nil {
				return nil, fmt.Errorf("mbox.backend.Storage.MailUnseen: %w", err)
			}
			status.Unseen = uint32(unseen)
		}
	}

	return status, nil
}

func (mbox *Mailbox) SetSubscribed(subscribed bool) error {
	return mbox.backend.Storage.MailboxSubscribe(mbox.name, subscribed)
}

func (mbox *Mailbox) Check() error {
	return nil
}

func (mbox *Mailbox) ListMessages(uid bool, seqSet *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer close(ch)

	ids, err := mbox.getIDsFromSeqSet(uid, seqSet)
	if err != nil {
		return fmt.Errorf("mbox.getIDsFromSeqSet: %w", err)
	}

	for _, id := range ids {
		mseq, mail, err := mbox.backend.Storage.MailSelect(mbox.name, int(id))
		if err != nil {
			continue
		}

		fetched := imap.NewMessage(uint32(id), items)
		fetched.SeqNum = uint32(mseq)
		fetched.Uid = uint32(mail.ID)

		// get function now supports both BLOB and file-based storage
		// Uses peek buffer (64 KB) for headers to avoid loading entire message
		get := func() (io.Reader, textproto.Header, error) {
			var reader io.Reader
			var closeFn func() error

			// Determine source: file or BLOB
			if mail.MailFile != "" {
				// Large message stored in file - use streaming
				file, err := mbox.backend.FileStore.ReadMail(mail.MailFile)
				if err != nil {
					return nil, textproto.Header{}, fmt.Errorf("FileStore.ReadMail: %w", err)
				}
				reader = file
				closeFn = file.Close
			} else {
				// Small message stored in BLOB
				reader = bytes.NewReader(mail.Mail)
				closeFn = func() error { return nil }
			}

			// Peek first 64 KB for headers (avoids loading entire large message)
			const peekSize = 64 * 1024
			peekBuf := make([]byte, peekSize)
			n, err := io.ReadFull(reader, peekBuf)
			if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
				closeFn()
				return nil, textproto.Header{}, fmt.Errorf("failed to peek headers: %w", err)
			}

			// Parse headers from peek buffer
			headerReader := bufio.NewReader(bytes.NewReader(peekBuf[:n]))
			hdr, err := textproto.ReadHeader(headerReader)
			if err != nil {
				closeFn()
				return nil, textproto.Header{}, fmt.Errorf("textproto.ReadHeader: %w", err)
			}

			// For body reading, need to combine remaining peek buffer + rest of stream
			// This creates a new reader that starts after headers
			bodyReader := io.MultiReader(headerReader, reader)

			// Wrap to ensure file gets closed when done
			wrappedReader := &readerWithCloser{reader: bodyReader, closeFn: closeFn}

			return wrappedReader, hdr, nil
		}

		for _, item := range items {
			switch item {
			case imap.FetchEnvelope:
				_, hdr, err := get()
				if err != nil {
					continue
				}
				if fetched.Envelope, err = backendutil.FetchEnvelope(hdr); err != nil {
					continue
				}

			case imap.FetchBody, imap.FetchBodyStructure:
				bodyreader, hdr, err := get()
				if err != nil {
					continue
				}
				if fetched.BodyStructure, err = backendutil.FetchBodyStructure(hdr, bodyreader, item == imap.FetchBodyStructure); err != nil {
					continue
				}

			case imap.FetchFlags:
				fetched.Flags = []string{}
				if mail.Seen {
					fetched.Flags = append(fetched.Flags, "\\Seen")
				}
				if mail.Answered {
					fetched.Flags = append(fetched.Flags, "\\Answered")
				}
				if mail.Flagged {
					fetched.Flags = append(fetched.Flags, "\\Flagged")
				}
				if mail.Deleted {
					fetched.Flags = append(fetched.Flags, "\\Deleted")
				}

			case imap.FetchInternalDate:
				fetched.InternalDate = mail.Date

			case imap.FetchRFC822Size:
				// Use mail.Size for both BLOB and file-based storage
			fetched.Size = uint32(mail.Size)

			case imap.FetchUid:
				fetched.Uid = uint32(id)

			default:
				section, err := imap.ParseBodySectionName(item)
				if err != nil {
					continue
				}
				bodyreader, hdr, err := get()
				if err != nil {
					continue
				}
				l, err := backendutil.FetchBodySection(hdr, bodyreader, section)
				if err != nil {
					continue
				}
				fetched.Body[section] = l
			}
		}

		ch <- fetched
	}

	return nil
}

func (mbox *Mailbox) SearchMessages(uid bool, criteria *imap.SearchCriteria) ([]uint32, error) {
	return mbox.backend.Storage.MailSearch(mbox.name)
}

func (mbox *Mailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	// Peek buffer to determine message size
	const peekSize = types.SmallMessageThreshold + 1
	peekBuf := make([]byte, peekSize)
	n, err := io.ReadFull(body, peekBuf)

	var id int

	if err == nil {
		// Message is >= 10MB+1, definitely large

		// Use streaming approach for large messages
		reader := io.MultiReader(bytes.NewReader(peekBuf[:n]), body)
		id, err = mbox.backend.Storage.MailCreateFromStream(mbox.name, reader, mbox.backend.FileStore)
		if err != nil {
			return fmt.Errorf("mbox.backend.Storage.MailCreateFromStream: %w", err)
		}
	} else if err == io.ErrUnexpectedEOF || err == io.EOF {
		// Message is smaller than peek buffer, use BLOB storage
		// Only use the bytes we actually read
		id, err = mbox.backend.Storage.MailCreate(mbox.name, peekBuf[:n])
		if err != nil {
			return fmt.Errorf("mbox.backend.Storage.MailCreate: %w", err)
		}
	} else {
		// Read error
		return fmt.Errorf("failed to read message body: %w", err)
	}

	// Set flags if specified
	for _, flag := range flags {
		var seen, answered, flagged, deleted bool
		switch flag {
		case "\\Seen":
			seen = true
		case "\\Answered":
			answered = true
		case "\\Flagged":
			flagged = true
		case "\\Deleted":
			deleted = true
		}
		if err := mbox.backend.Storage.MailUpdateFlags(
			mbox.name, id, seen, answered, flagged, deleted,
		); err != nil {
			return err
		}
	}

	return nil
}

func (mbox *Mailbox) UpdateMessagesFlags(uid bool, seqSet *imap.SeqSet, op imap.FlagsOp, flags []string) error {
	ids, err := mbox.getIDsFromSeqSet(uid, seqSet)
	if err != nil {
		return fmt.Errorf("mbox.getIDsFromSeqSet: %w", err)
	}

	for _, id := range ids {
		var mail *types.Mail
		if op != imap.SetFlags {
			var err error
			_, mail, err = mbox.backend.Storage.MailSelect(mbox.name, int(id))
			if err != nil {
				return fmt.Errorf("mbox.backend.Storage.MailSelect: %w", err)
			}
		}
		for _, flag := range flags {
			switch flag {
			case "\\Seen":
				mail.Seen = op != imap.RemoveFlags
			case "\\Answered":
				mail.Answered = op != imap.RemoveFlags
			case "\\Flagged":
				mail.Flagged = op != imap.RemoveFlags
			case "\\Deleted":
				mail.Deleted = op != imap.RemoveFlags
			}
		}

		if err := mbox.backend.Storage.MailUpdateFlags(
			mbox.name, int(mail.ID), mail.Seen,
			mail.Answered, mail.Flagged, mail.Deleted,
		); err != nil {
			return err
		}
	}
	return nil
}

func (mbox *Mailbox) CopyMessages(uid bool, seqSet *imap.SeqSet, destName string) error {
	if destName == "Outbox" {
		return fmt.Errorf("can't copy into Outbox as it is a protected folder")
	}

	ids, err := mbox.getIDsFromSeqSet(uid, seqSet)
	if err != nil {
		return fmt.Errorf("mbox.getIDsFromSeqSet: %w", err)
	}

	for _, id := range ids {
		_, mail, err := mbox.backend.Storage.MailSelect(mbox.name, int(id))
		if err != nil {
			return fmt.Errorf("mbox.backend.Storage.MailSelect: %w", err)
		}

		var pid int

		// Handle both file-based and BLOB-based messages
		if mail.MailFile != "" {
			// Large message stored in file - use streaming copy
			file, err := mbox.backend.FileStore.ReadMail(mail.MailFile)
			if err != nil {
				return fmt.Errorf("FileStore.ReadMail: %w", err)
			}
			defer file.Close()

			pid, err = mbox.backend.Storage.MailCreateFromStream(destName, file, mbox.backend.FileStore)
			if err != nil {
				return fmt.Errorf("mbox.backend.Storage.MailCreateFromStream: %w", err)
			}
		} else {
			// Small message stored in BLOB
			pid, err = mbox.backend.Storage.MailCreate(destName, mail.Mail)
			if err != nil {
				return fmt.Errorf("mbox.backend.Storage.MailCreate: %w", err)
			}
		}

		// Copy flags to the new message
		if err = mbox.backend.Storage.MailUpdateFlags(
			destName, pid, mail.Seen, mail.Answered, mail.Flagged, mail.Deleted,
		); err != nil {
			return fmt.Errorf("mbox.backend.Storage.MailUpdateFlags: %w", err)
		}
	}
	return nil
}

func (mbox *Mailbox) Expunge() error {
	return mbox.backend.Storage.MailExpunge(mbox.name)
}

func (mbox *Mailbox) MoveMessages(uid bool, seqset *imap.SeqSet, dest string) error {
	if dest == "Outbox" {
		return fmt.Errorf("can't copy into Outbox as it is a protected folder")
	}

	ids, err := mbox.getIDsFromSeqSet(uid, seqset)
	if err != nil {
		return fmt.Errorf("mbox.getIDsFromSeqSet: %w", err)
	}

	for _, id := range ids {
		if err := mbox.backend.Storage.MailMove(mbox.name, int(id), dest); err != nil {
			return err
		}
	}
	return nil
}
