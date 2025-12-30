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
	"strings"
	"testing"
)

func TestIsValidDomain(t *testing.T) {
	tests := []struct {
		name   string
		domain string
		valid  bool
	}{
		{"yggmail domain", "yggmail", true},
		{"yggmail.local domain", "yggmail.local", true},
		{"uppercase YGGMAIL", "YGGMAIL", true},
		{"mixed case Yggmail", "Yggmail", true},
		{"uppercase YGGMAIL.LOCAL", "YGGMAIL.LOCAL", true},
		{"mixed case Yggmail.Local", "Yggmail.Local", true},
		{"with leading space", " yggmail", true},
		{"with trailing space", "yggmail ", true},
		{"with both spaces", " yggmail.local ", true},
		{"invalid yggmail.com", "yggmail.com", false},
		{"invalid gmail.com", "gmail.com", false},
		{"invalid example.com", "example.com", false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsValidDomain(tt.domain)
			if result != tt.valid {
				t.Errorf("IsValidDomain(%q) = %v, want %v", tt.domain, result, tt.valid)
			}
		})
	}
}

func TestParseAddressBothDomains(t *testing.T) {
	// Generate a test Ed25519 key pair
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("Failed to generate key: %v", err)
	}
	pk := hex.EncodeToString(pub)

	tests := []struct {
		name    string
		email   string
		wantErr bool
	}{
		{"valid @yggmail", pk + "@yggmail", false},
		{"valid @yggmail.local", pk + "@yggmail.local", false},
		{"uppercase @YGGMAIL", pk + "@YGGMAIL", false},
		{"uppercase @YGGMAIL.LOCAL", pk + "@YGGMAIL.LOCAL", false},
		{"mixed case @Yggmail.Local", pk + "@Yggmail.Local", false},
		{"with leading space", " " + pk + "@yggmail", false},
		{"with trailing space", pk + "@yggmail ", false},
		{"with both spaces", " " + pk + "@yggmail.local ", false},
		{"invalid @yggmail.com", pk + "@yggmail.com", true},
		{"invalid @gmail.com", pk + "@gmail.com", true},
		{"no @ symbol", pk, true},
		{"@ at start", "@yggmail", true},
		{"empty string", "", true},
		{"invalid hex in local part", "gggggg@yggmail", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseAddress(tt.email)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseAddress(%q) expected error, got nil", tt.email)
				}
			} else {
				if err != nil {
					t.Errorf("ParseAddress(%q) unexpected error: %v", tt.email, err)
				}
				if result == nil {
					t.Errorf("ParseAddress(%q) returned nil public key", tt.email)
				}
				// Verify the parsed key matches the original
				if !strings.EqualFold(hex.EncodeToString(result), pk) {
					t.Errorf("ParseAddress(%q) returned wrong key: got %s, want %s",
						tt.email, hex.EncodeToString(result), pk)
				}
			}
		})
	}
}

func TestCreateAddress(t *testing.T) {
	// Generate a test Ed25519 key pair
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("Failed to generate key: %v", err)
	}

	addr := CreateAddress(pub)
	expectedSuffix := "@yggmail"

	if !strings.HasSuffix(addr, expectedSuffix) {
		t.Errorf("CreateAddress() = %q, want suffix %q", addr, expectedSuffix)
	}

	// Verify the created address can be parsed back
	parsed, err := ParseAddress(addr)
	if err != nil {
		t.Errorf("Failed to parse created address: %v", err)
	}

	if !pub.Equal(parsed) {
		t.Errorf("Parsed key doesn't match original: got %s, want %s",
			hex.EncodeToString(parsed), hex.EncodeToString(pub))
	}
}

func TestParseAddressRoundtrip(t *testing.T) {
	// Test that CreateAddress and ParseAddress are inverse operations
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("Failed to generate key: %v", err)
	}

	// Create address with @yggmail
	addr := CreateAddress(pub)

	// Parse it back
	parsed, err := ParseAddress(addr)
	if err != nil {
		t.Errorf("Failed to parse created address: %v", err)
	}

	if !pub.Equal(parsed) {
		t.Errorf("Roundtrip failed: got %s, want %s",
			hex.EncodeToString(parsed), hex.EncodeToString(pub))
	}

	// Also test with @yggmail.local alias
	addrLocal := strings.Replace(addr, "@yggmail", "@yggmail.local", 1)
	parsedLocal, err := ParseAddress(addrLocal)
	if err != nil {
		t.Errorf("Failed to parse address with .local: %v", err)
	}

	if !pub.Equal(parsedLocal) {
		t.Errorf("Roundtrip with .local failed: got %s, want %s",
			hex.EncodeToString(parsedLocal), hex.EncodeToString(pub))
	}
}

func TestParseAddressErrorMessages(t *testing.T) {
	tests := []struct {
		name        string
		email       string
		expectedMsg string
	}{
		{
			"invalid domain",
			"abc123@gmail.com",
			"invalid email domain",
		},
		{
			"no @ symbol",
			"abc123",
			"invalid email address",
		},
		{
			"invalid hex",
			"gggggg@yggmail",
			"hex.DecodeString",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseAddress(tt.email)
			if err == nil {
				t.Errorf("ParseAddress(%q) expected error, got nil", tt.email)
				return
			}
			if !strings.Contains(err.Error(), tt.expectedMsg) {
				t.Errorf("ParseAddress(%q) error = %q, want to contain %q",
					tt.email, err.Error(), tt.expectedMsg)
			}
		})
	}
}
