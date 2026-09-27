-- Atomic sliding-window rate limiter.
--
-- The equivalent Go code used to issue LLEN, then LINDEX, then LPUSH as separate
-- round trips. Between the read and the write, every concurrent caller observed
-- the same under-limit length and every one of them was admitted, so the limiter
-- only ever constrained sequential traffic. Running the whole decision inside
-- Redis makes it atomic.
--
-- A limit that only counts requests once they have succeeded cannot record at
-- admission time, so the decision and the record can also run on their own:
-- "check" decides without recording, "record" appends without deciding, and
-- "allow" does both in one step.
--
-- KEYS[1] bucket key
-- ARGV[1] maximum requests allowed inside the window
-- ARGV[2] window length in seconds
-- ARGV[3] current unix time in seconds
-- ARGV[4] key ttl in seconds
-- ARGV[5] mode: "allow", "check" or "record"
--
-- Returns 1 when the request is admitted, 0 when it is rejected. A non-positive
-- maximum rejects every request and records nothing.

local key    = KEYS[1]
local maxNum = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local now    = tonumber(ARGV[3])
local ttl    = tonumber(ARGV[4])
local mode   = ARGV[5]

if maxNum <= 0 then
  return 0
end

if mode ~= 'record' then
  if redis.call('LLEN', key) >= maxNum then
    -- Entries written by the previous implementation are formatted timestamps
    -- rather than numbers. tonumber yields nil for those, and treating that as
    -- "window has rolled" lets the key heal itself within one window instead of
    -- erroring.
    local oldest = tonumber(redis.call('LINDEX', key, -1))
    if oldest ~= nil and (now - oldest) < window then
      redis.call('EXPIRE', key, ttl)
      return 0
    end
  end
  if mode == 'check' then
    return 1
  end
end

-- LPUSH returns the new length, so only a list that has run past the maximum is
-- trimmed. Numbers handed back to Redis are converted through a double, and a
-- maximum near the int64 range becomes a string LTRIM rejects; no list gets that
-- long, so such a maximum never reaches LTRIM.
if redis.call('LPUSH', key, now) > maxNum then
  redis.call('LTRIM', key, 0, maxNum - 1)
end
redis.call('EXPIRE', key, ttl)
return 1
