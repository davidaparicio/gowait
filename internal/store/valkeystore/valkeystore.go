// Package valkeystore implements store.Store on Valkey (or any
// Redis-compatible server), so multiple gowait replicas can share one
// waiting room.
//
// Every store.Store method runs as a single server-side Lua script, keeping
// each operation atomic across replicas — except Set/GetCapacity, which are
// plain single commands (individually atomic already). Data model (all keys
// under a configurable prefix):
//
//	<p>order    ZSET  queued ids scored by a monotonic sequence → FIFO + rank
//	<p>seen     ZSET  queued ids scored by lastSeen ms → ghost eviction
//	<p>active   ZSET  active ids scored by lastSeen ms → idle expiry
//	<p>admitted HASH  id → admittedAt ms, for session-duration EMA
//	<p>avg      STRING EMA of completed session durations (seconds)
//	<p>seq      STRING monotonic enqueue counter
//	<p>capacity STRING runtime capacity override, absent = none
package valkeystore

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/davidaparicio/gowait/internal/store"
)

// Status codes returned by the scripts, matching store.Status values
// (0 unknown, 1 queued, 2 active).

var tryAdmitScript = valkey.NewLuaScript(`
local id, cap, now = ARGV[1], tonumber(ARGV[2]), ARGV[3]
if redis.call('ZSCORE', KEYS[1], id) then
  redis.call('ZADD', KEYS[1], now, id)
  return 1
end
if redis.call('ZCARD', KEYS[1]) < cap and redis.call('ZCARD', KEYS[3]) == 0 then
  redis.call('ZADD', KEYS[1], now, id)
  redis.call('HSET', KEYS[2], id, now)
  return 1
end
return 0
`)

var enqueueScript = valkey.NewLuaScript(`
local id, now = ARGV[1], ARGV[2]
if redis.call('ZSCORE', KEYS[3], id) then
  return {2, 0, redis.call('ZCARD', KEYS[1]), redis.call('ZCARD', KEYS[3])}
end
local rank = redis.call('ZRANK', KEYS[1], id)
if not rank then
  local seq = redis.call('INCR', KEYS[4])
  redis.call('ZADD', KEYS[1], seq, id)
  rank = redis.call('ZRANK', KEYS[1], id)
end
redis.call('ZADD', KEYS[2], now, id)
return {1, rank + 1, redis.call('ZCARD', KEYS[1]), redis.call('ZCARD', KEYS[3])}
`)

var lookupScript = valkey.NewLuaScript(`
local id, now = ARGV[1], ARGV[2]
local qlen, act = redis.call('ZCARD', KEYS[1]), redis.call('ZCARD', KEYS[3])
if redis.call('ZSCORE', KEYS[3], id) then
  redis.call('ZADD', KEYS[3], now, id)
  return {2, 0, qlen, act}
end
local rank = redis.call('ZRANK', KEYS[1], id)
if rank then
  redis.call('ZADD', KEYS[2], now, id)
  return {1, rank + 1, qlen, act}
end
return {0, 0, qlen, act}
`)

var touchScript = valkey.NewLuaScript(`
if redis.call('ZSCORE', KEYS[1], ARGV[1]) then
  redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
  return 1
end
return 0
`)

var reconcileScript = valkey.NewLuaScript(`
local cap = tonumber(ARGV[1])
local activeTTL, queueTTL, now = tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])

-- 1. Expire idle actives, feeding the session-duration EMA.
local expired = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', '(' .. (now - activeTTL))
for _, id in ipairs(expired) do
  local lastSeen = tonumber(redis.call('ZSCORE', KEYS[1], id))
  local admittedAt = tonumber(redis.call('HGET', KEYS[2], id)) or lastSeen
  local dur = (lastSeen - admittedAt) / 1000.0
  local avg = tonumber(redis.call('GET', KEYS[5]))
  if not avg or avg == 0 then avg = dur else avg = 0.2 * dur + 0.8 * avg end
  redis.call('SET', KEYS[5], tostring(avg))
  redis.call('ZREM', KEYS[1], id)
  redis.call('HDEL', KEYS[2], id)
end

-- 2. Evict ghost queuers that stopped polling.
local ghosts = redis.call('ZRANGEBYSCORE', KEYS[4], '-inf', '(' .. (now - queueTTL))
for _, id in ipairs(ghosts) do
  redis.call('ZREM', KEYS[3], id)
  redis.call('ZREM', KEYS[4], id)
end

-- 3. Promote queue heads into free slots.
local promoted = 0
while redis.call('ZCARD', KEYS[1]) < cap do
  local head = redis.call('ZPOPMIN', KEYS[3])
  if #head == 0 then break end
  local id = head[1]
  redis.call('ZREM', KEYS[4], id)
  redis.call('ZADD', KEYS[1], now, id)
  redis.call('HSET', KEYS[2], id, now)
  promoted = promoted + 1
end
return promoted
`)

var statsScript = valkey.NewLuaScript(`
return {redis.call('ZCARD', KEYS[1]), redis.call('ZCARD', KEYS[2]),
        redis.call('GET', KEYS[3]) or ''}
`)

type Store struct {
	client valkey.Client
	// key names, precomputed
	order, seen, active, admitted, avg, seq, capacityKey string
}

// New connects to Valkey at url (valkey://, redis:// or plain host:port) and
// namespaces all keys under prefix. For Valkey Cluster, use a prefix with a
// hash tag (e.g. "{gowait}:") so all keys land in one slot.
func New(url, prefix string) (*Store, error) {
	opt, err := valkey.ParseURL(url)
	if err != nil {
		// Not a URL; treat as a plain host:port address.
		opt = valkey.ClientOption{InitAddress: []string{url}}
	}
	client, err := valkey.NewClient(opt)
	if err != nil {
		return nil, fmt.Errorf("connecting to valkey at %s: %w", url, err)
	}
	return NewWithClient(client, prefix), nil
}

// NewWithClient wraps an existing client (useful for tests).
func NewWithClient(client valkey.Client, prefix string) *Store {
	return &Store{
		client:      client,
		order:       prefix + "order",
		seen:        prefix + "seen",
		active:      prefix + "active",
		admitted:    prefix + "admitted",
		avg:         prefix + "avg",
		seq:         prefix + "seq",
		capacityKey: prefix + "capacity",
	}
}

func (s *Store) Close() { s.client.Close() }

func ms(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

func (s *Store) TryAdmit(ctx context.Context, id string, capacity int, now time.Time) (bool, error) {
	n, err := tryAdmitScript.Exec(ctx, s.client,
		[]string{s.active, s.admitted, s.order},
		[]string{id, strconv.Itoa(capacity), ms(now)}).AsInt64()
	if err != nil {
		return false, fmt.Errorf("valkey TryAdmit: %w", err)
	}
	return n == 1, nil
}

func (s *Store) Enqueue(ctx context.Context, id string, now time.Time) (store.Snapshot, error) {
	res := enqueueScript.Exec(ctx, s.client,
		[]string{s.order, s.seen, s.active, s.seq},
		[]string{id, ms(now)})
	return parseSnapshot(res, "Enqueue")
}

func (s *Store) Lookup(ctx context.Context, id string, now time.Time) (store.Snapshot, error) {
	res := lookupScript.Exec(ctx, s.client,
		[]string{s.order, s.seen, s.active},
		[]string{id, ms(now)})
	return parseSnapshot(res, "Lookup")
}

func (s *Store) Touch(ctx context.Context, id string, now time.Time) (bool, error) {
	n, err := touchScript.Exec(ctx, s.client,
		[]string{s.active},
		[]string{id, ms(now)}).AsInt64()
	if err != nil {
		return false, fmt.Errorf("valkey Touch: %w", err)
	}
	return n == 1, nil
}

func (s *Store) Reconcile(ctx context.Context, capacity int, activeTTL, queueTTL time.Duration, now time.Time) (int, error) {
	n, err := reconcileScript.Exec(ctx, s.client,
		[]string{s.active, s.admitted, s.order, s.seen, s.avg},
		[]string{
			strconv.Itoa(capacity),
			strconv.FormatInt(activeTTL.Milliseconds(), 10),
			strconv.FormatInt(queueTTL.Milliseconds(), 10),
			ms(now),
		}).AsInt64()
	if err != nil {
		return 0, fmt.Errorf("valkey Reconcile: %w", err)
	}
	return int(n), nil
}

func (s *Store) Stats(ctx context.Context) (store.Stats, error) {
	arr, err := statsScript.Exec(ctx, s.client,
		[]string{s.order, s.active, s.avg}, nil).ToArray()
	if err != nil || len(arr) != 3 {
		return store.Stats{}, fmt.Errorf("valkey Stats: %w", err)
	}
	qlen, err := arr[0].AsInt64()
	if err != nil {
		return store.Stats{}, fmt.Errorf("valkey Stats queue length: %w", err)
	}
	act, err := arr[1].AsInt64()
	if err != nil {
		return store.Stats{}, fmt.Errorf("valkey Stats active count: %w", err)
	}
	stats := store.Stats{QueueLength: int(qlen), ActiveCount: int(act)}
	if avgStr, err := arr[2].ToString(); err == nil && avgStr != "" {
		if avg, err := strconv.ParseFloat(avgStr, 64); err == nil {
			stats.AvgSessionSecs = avg
		}
	}
	return stats, nil
}

func (s *Store) SetCapacity(ctx context.Context, capacity int) error {
	err := s.client.Do(ctx,
		s.client.B().Set().Key(s.capacityKey).Value(strconv.Itoa(capacity)).Build()).Error()
	if err != nil {
		return fmt.Errorf("valkey SetCapacity: %w", err)
	}
	return nil
}

func (s *Store) GetCapacity(ctx context.Context) (int, bool, error) {
	v, err := s.client.Do(ctx, s.client.B().Get().Key(s.capacityKey).Build()).ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("valkey GetCapacity: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false, fmt.Errorf("valkey GetCapacity: bad value %q: %w", v, err)
	}
	return n, true, nil
}

func parseSnapshot(res valkey.ValkeyResult, op string) (store.Snapshot, error) {
	arr, err := res.ToArray()
	if err != nil || len(arr) != 4 {
		return store.Snapshot{}, fmt.Errorf("valkey %s: %w", op, err)
	}
	vals := make([]int64, 4)
	for i, m := range arr {
		if vals[i], err = m.AsInt64(); err != nil {
			return store.Snapshot{}, fmt.Errorf("valkey %s field %d: %w", op, i, err)
		}
	}
	return store.Snapshot{
		Status:      store.Status(vals[0]),
		Position:    int(vals[1]),
		QueueLength: int(vals[2]),
		ActiveCount: int(vals[3]),
	}, nil
}
