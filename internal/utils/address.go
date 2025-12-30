/*
 *  Copyright (c) 2021 Neil Alexander
 *
 *  This Source Code Form is subject to the terms of the Mozilla Public
 *  License, v. 2.0. If a copy of the MPL was not distributed with this
 *  file, You can obtain one at http://mozilla.org/MPL/2.0/.
 */

package utils

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"strings"
)

const Domain = "yggmail"
const DomainAlias = "yggmail.local"

// IsValidDomain checks if domain is either "yggmail" or "yggmail.local" (case-insensitive)
func IsValidDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	return d == Domain || d == DomainAlias
}

func CreateAddress(pk ed25519.PublicKey) string {
	return fmt.Sprintf(
		"%s@%s",
		hex.EncodeToString(pk), Domain,
	)
}

func ParseAddress(email string) (ed25519.PublicKey, error) {
	email = strings.TrimSpace(email)
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return nil, fmt.Errorf("invalid email address")
	}

	domain := email[at+1:]
	if !IsValidDomain(domain) {
		return nil, fmt.Errorf("invalid email domain: expected %s or %s, got %s",
			Domain, DomainAlias, domain)
	}

	pk, err := hex.DecodeString(email[:at])
	if err != nil {
		return nil, fmt.Errorf("hex.DecodeString: %w", err)
	}
	ed := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(ed, pk)
	return ed, nil
}
