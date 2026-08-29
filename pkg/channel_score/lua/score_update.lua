-- Applies one outcome to a channel score as a single atomic transition.
--
-- The whole record moves together on purpose. Rotating the success window,
-- resetting the opposing streak and clamping the tier offset are one decision;
-- issuing them as separate HINCRBY/HSET commands lets two instances interleave
-- into a state neither of them intended (a fault that clears a streak the other
-- instance just extended, a window rotated twice, an offset clamped against a
-- stale read).
--
-- KEYS[1] = score hash key
-- ARGV: 1 outcome (1=success, 2=fault)
--       2 now (unix seconds)
--       3 halfWidth (seconds)
--       4 successesToPromote
--       5 faultsToDemote
--       6 maxPromoteTiers
--       7 maxDemoteTiers
--       8 idleResetSeconds
--       9 ttlSeconds
-- Returns: {tierOffset, total, success}

local key = KEYS[1]
local outcome = tonumber(ARGV[1])
local now = tonumber(ARGV[2])
local halfWidth = tonumber(ARGV[3])
local successesToPromote = tonumber(ARGV[4])
local faultsToDemote = tonumber(ARGV[5])
local maxPromoteTiers = tonumber(ARGV[6])
local maxDemoteTiers = tonumber(ARGV[7])
local idleResetSeconds = tonumber(ARGV[8])
local ttlSeconds = tonumber(ARGV[9])

local raw = redis.call('HMGET', key,
  'cs', 'fc', 'off', 'cst', 'ct', 'csu', 'pt', 'psu', 'ua')

local consecutiveSuccess = tonumber(raw[1]) or 0
local faultCount = tonumber(raw[2]) or 0
local tierOffset = tonumber(raw[3]) or 0
local curStart = tonumber(raw[4]) or 0
local curTotal = tonumber(raw[5]) or 0
local curSuccess = tonumber(raw[6]) or 0
local prevTotal = tonumber(raw[7]) or 0
local prevSuccess = tonumber(raw[8]) or 0
local updatedAt = tonumber(raw[9]) or 0

-- Idle reset. Judged inside the script so a stale read cannot resurrect an
-- expired record: a caller that decided "this is idle" seconds ago would
-- otherwise overwrite an update another instance just made.
if updatedAt > 0 and idleResetSeconds > 0 and (now - updatedAt) >= idleResetSeconds then
  consecutiveSuccess = 0
  faultCount = 0
  tierOffset = 0
  curStart = 0
  curTotal = 0
  curSuccess = 0
  prevTotal = 0
  prevSuccess = 0
end

-- Rotate the sliding window.
if halfWidth > 0 then
  if curStart == 0 then
    curStart = now
  else
    local elapsed = now - curStart
    if elapsed >= halfWidth then
      if elapsed < (2 * halfWidth) then
        prevTotal = curTotal
        prevSuccess = curSuccess
      else
        prevTotal = 0
        prevSuccess = 0
      end
      curTotal = 0
      curSuccess = 0
      curStart = now
    end
  end
end

curTotal = curTotal + 1

if outcome == 1 then
  curSuccess = curSuccess + 1
  -- A success ends any fault streak: demotion counts consecutive faults, so a
  -- channel that recovers must not carry half a demotion forward.
  faultCount = 0
  consecutiveSuccess = consecutiveSuccess + 1
  if successesToPromote > 0 and consecutiveSuccess >= successesToPromote then
    consecutiveSuccess = 0
    if tierOffset < maxPromoteTiers then
      tierOffset = tierOffset + 1
    end
  end
else
  consecutiveSuccess = 0
  faultCount = faultCount + 1
  if faultsToDemote > 0 and faultCount >= faultsToDemote then
    faultCount = 0
    if tierOffset > -maxDemoteTiers then
      tierOffset = tierOffset - 1
    end
  end
end

redis.call('HMSET', key,
  'cs', consecutiveSuccess,
  'fc', faultCount,
  'off', tierOffset,
  'cst', curStart,
  'ct', curTotal,
  'csu', curSuccess,
  'pt', prevTotal,
  'psu', prevSuccess,
  'ua', now)

if ttlSeconds > 0 then
  redis.call('EXPIRE', key, ttlSeconds)
end

return {tierOffset, curTotal + prevTotal, curSuccess + prevSuccess}
