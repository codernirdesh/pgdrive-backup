package main

import "testing"

func TestDefaultDecryptedName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "pgbackup_mydb_2026-04-19_020000.sql.gz.enc", want: "pgbackup_mydb_2026-04-19_020000.dump"},
		{name: "already.dump.enc", want: "already.dump"},
		{name: ".sql.gz.enc", want: "restore.dump"},
	}

	for _, tt := range tests {
		if got := decryptedName(tt.name); got != tt.want {
			t.Fatalf("decryptedName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		size int64
		want string
	}{
		{size: 0, want: "—"},
		{size: 1, want: "1 B"},
		{size: 1024, want: "1.0 KB"},
		{size: 3 * 1024 * 1024, want: "3.0 MB"},
	}

	for _, tt := range tests {
		if got := humanSize(tt.size); got != tt.want {
			t.Fatalf("humanSize(%d) = %q, want %q", tt.size, got, tt.want)
		}
	}
}
