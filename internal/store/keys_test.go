package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func hashOf(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func TestKeysLifecycle(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 100); err != nil {
		t.Fatal(err)
	}

	plain, key, err := d.CreateKey(ctx, 100, "Claude Code")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(plain) != 67 || plain[:3] != "sk-" {
		t.Fatalf("plaintext key shape = %q", plain)
	}
	if len(key.Prefix) != 11 || !strings.HasPrefix(key.Prefix, "sk-") {
		t.Fatalf("prefix = %q, want %q", key.Prefix, plain[:11])
	}

	keys, err := d.ListKeys(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "Claude Code" {
		t.Fatalf("keys = %+v", keys)
	}

	got, err := d.KeyByHash(ctx, hashOf(plain))
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ID != key.ID || got.TelegramID != 100 {
		t.Fatalf("lookup = %+v, want id %d", got, key.ID)
	}

	if err := d.RevokeKey(ctx, 100, key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := d.KeyByHash(ctx, hashOf(plain)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked key still resolves: %v", err)
	}
	if err := d.RevokeKey(ctx, 100, key.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke = %v, want ErrNotFound", err)
	}
}

func TestKeyLimit(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.EnsureCustomer(ctx, 101); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxActiveKeys; i++ {
		if _, _, err := d.CreateKey(ctx, 101, "k"); err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	if _, _, err := d.CreateKey(ctx, 101, "k"); !errors.Is(err, ErrKeyCap) {
		t.Fatalf("sixth key = %v, want ErrKeyCap", err)
	}
}
