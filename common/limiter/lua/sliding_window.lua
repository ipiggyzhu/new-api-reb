-- Atomic sliding-window rate limiter.
--
-- The equivalent Go code used to issue LLEN, then LINDEX, then LPUSH as separate
-- round trips. Between the read and the write, every concurrent caller observed
-- the same under-limit length and every one of them was admitted, so the limiter
-- only ever constrained sequential traffic. Running the whole decision inside
-- Redis makes it atomic.
--
-- KEYS[1] bucket key
-- ARGV[1] maximum requests allowed inside the window
-- ARGV[2] window length in seconds
-- ARGV[3] current unix time in seconds
-- ARGV[4] key ttl in seconds
--
-- Returns 1 when the request is admitted, 0 when it is rejected.

local key    = KEYS[1]
local maxNum = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local now    = tonumber(ARGV[3])
local ttl    = tonumber(ARGV[4])

if maxNum <= 0 then
  return 0
end

local length = redis.call('LLEN', key)
if length < maxNum then
  redis.call('LPUSH', key, now)
  redis.call('EXPIRE', key, ttl)
  return 1
end

-- Entries written by the previous implementation are formatted timestamps rather
-- than numbers. tonumber yields nil for those, and treating that as "window has
-- rolled" lets the key heal itself within one window instead of erroring.
local oldest = tonumber(redis.call('LINDEX', key, -1))
if oldest ~= nil and (now - oldest) < window then
  redis.call('EXPIRE', key, ttl)
  return 0
end

redis.call('LPUSH', key, now)
redis.call('LTRIM', key, 0, maxNum - 1)
redis.call('EXPIRE', key, ttl)
return 1
