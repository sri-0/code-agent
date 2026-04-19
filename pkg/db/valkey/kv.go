package valkey

import (
	"context"
	"time"
)

// Set stores a key-value pair with optional TTL (0 = no expiry).
func (v *Valkey) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if ttl > 0 {
		return v.Client.Do(ctx, v.Client.B().Set().Key(key).Value(value).Ex(ttl).Build()).Error()
	}
	return v.Client.Do(ctx, v.Client.B().Set().Key(key).Value(value).Build()).Error()
}

// Get retrieves the value of a key.
func (v *Valkey) Get(ctx context.Context, key string) (string, error) {
	return v.Client.Do(ctx, v.Client.B().Get().Key(key).Build()).ToString()
}

// Del deletes one or more keys.
func (v *Valkey) Del(ctx context.Context, keys ...string) error {
	return v.Client.Do(ctx, v.Client.B().Del().Key(keys...).Build()).Error()
}

// SAdd adds members to a set.
func (v *Valkey) SAdd(ctx context.Context, key string, members ...string) error {
	return v.Client.Do(ctx, v.Client.B().Sadd().Key(key).Member(members...).Build()).Error()
}

// SMembers returns all members of a set.
func (v *Valkey) SMembers(ctx context.Context, key string) ([]string, error) {
	return v.Client.Do(ctx, v.Client.B().Smembers().Key(key).Build()).AsStrSlice()
}

// SRem removes members from a set.
func (v *Valkey) SRem(ctx context.Context, key string, members ...string) error {
	return v.Client.Do(ctx, v.Client.B().Srem().Key(key).Member(members...).Build()).Error()
}
