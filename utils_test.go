package main

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d        time.Duration
		expected string
	}{
		{5 * time.Second, "5 seconds"},
		{1 * time.Second, "1 second"},
		{65 * time.Second, "1 minute, 5 seconds"},
		{3600 * time.Second, "1 hour"},
		{3661 * time.Second, "1 hour, 1 minute, 1 second"},
		{time.Millisecond, "0 seconds"}, // Rounds down to 0
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, formatDuration(tt.d))
		})
	}
}

func TestInjectPassword(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		password string
		expected string
	}{
		{"Standard Replace", "postgres://user:old@host:5432/db", "newpass", "postgres://user:newpass@host:5432/db"},
		{"Add to User Only", "mysql://user@host:3306/db", "secret", "mysql://user:secret@host:3306/db"},
		{"No URI", "", "pass", ""},
		{"No Password", "postgres://user:pass@host/db", "", "postgres://user:pass@host/db"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, injectPassword(tt.uri, tt.password))
		})
	}
}

func TestExtractDBName(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		expected string
	}{
		{"Standard URI", "postgres://user:pass@host:5432/my_database", "my_database"},
		{"MySQL URI", "mysql://user:pass@host:3306/another_db", "another_db"},
		{"DocumentDB URI", "mongodb://user:pass@host:27017/doc_db", "doc_db"},
		{"No Database", "postgres://user:pass@host:5432/", ""},
		{"Empty URI", "", ""},
		{"Invalid URI", ":/invalid/", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractDBName(tt.uri))
		})
	}
}

func TestGetDirSize(t *testing.T) {
	dir := t.TempDir()

	// Create dummy files
	err := os.WriteFile(dir+"/test1.txt", []byte("12345"), 0644) // 5 bytes
	require.NoError(t, err)
	err = os.WriteFile(dir+"/test2.txt", []byte("1234567890"), 0644) // 10 bytes
	require.NoError(t, err)

	size, err := getDirSize(dir)
	require.NoError(t, err)

	// Ensure size equals the payload written
	assert.Equal(t, int64(15), size)
}
