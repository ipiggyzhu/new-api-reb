// Package privacy keeps reversible prompt substitutions inside the gateway.
// References carry random identifiers, never an encoding or hash of a secret.
package privacy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
)

const (
	ContextKey     = "new_api_privacy_session"
	Prefix         = "__NA_PRIV_"
	MaxSecrets     = 256
	MaxSecretBytes = 32 << 10
)

var (
	ErrLimit       = errors.New("privacy limits exceeded")
	ErrSession     = errors.New("privacy session expired or unavailable; resend the full conversation without the session header")
	ErrUnsupported = errors.New("privacy supports text JSON requests only; opaque content requires local sanitization")
)

type Session struct {
	mu         sync.Mutex
	Values     map[string]string `json:"values"` // random reference -> plaintext, request lifetime only
	ProtectIPs bool              `json:"-"`
	// BestEffort marks a request protected by the site-wide switch rather than
	// by its own header: what cannot be inspected passes through instead of
	// failing the request.
	BestEffort bool `json:"-"`
}

func FromContext(c *gin.Context) *Session {
	v, _ := c.Get(ContextKey)
	s, _ := v.(*Session)
	return s
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("privacy randomness unavailable")
	}
	return hex.EncodeToString(b[:]), nil
}

func NewSession() *Session { return &Session{Values: make(map[string]string)} }

func (s *Session) Clear() {
	s.mu.Lock()
	clear(s.Values)
	s.Values = make(map[string]string)
	s.mu.Unlock()
}

func (s *Session) Add(value string) error {
	if value == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, isReference := s.Values[value]; isReference {
		return nil
	}
	for _, existing := range s.Values {
		if existing == value {
			return nil
		}
	}
	if len(s.Values) >= MaxSecrets || len(value) > MaxSecretBytes {
		return ErrLimit
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	s.Values[Prefix+id+"__"] = value
	return nil
}

func (s *Session) snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make(map[string]string, len(s.Values))
	for ref, value := range s.Values {
		values[ref] = value
	}
	return values
}

var referencePattern = regexp.MustCompile(`__NA_PRIV_[0-9a-f]{32}__`)

func (s *Session) Mask(text string) string {
	values := s.snapshot()
	refs := make([]string, 0, len(values))
	for ref := range values {
		refs = append(refs, ref)
	}
	// Long values must win when one credential contains another credential/IP.
	sort.Slice(refs, func(i, j int) bool {
		if len(values[refs[i]]) == len(values[refs[j]]) {
			return refs[i] < refs[j]
		}
		return len(values[refs[i]]) > len(values[refs[j]])
	})
	pairs := make([]string, 0, len(refs)*2)
	for _, ref := range refs {
		pairs = append(pairs, values[ref], ref)
	}
	if len(pairs) == 0 {
		return text
	}
	// Replacer makes one pass, so a short secret cannot corrupt a new reference.
	replacer := strings.NewReplacer(pairs...)
	var out strings.Builder
	start := 0
	for _, match := range referencePattern.FindAllStringIndex(text, -1) {
		reference := text[match[0]:match[1]]
		if _, issued := values[reference]; !issued {
			continue
		}
		out.WriteString(replacer.Replace(text[start:match[0]]))
		out.WriteString(reference)
		start = match[1]
	}
	out.WriteString(replacer.Replace(text[start:]))
	return out.String()
}

func (s *Session) Restore(text string) string {
	values := s.snapshot()
	pairs := make([]string, 0, len(values)*2)
	for ref, value := range values {
		pairs = append(pairs, ref, value)
	}
	if len(pairs) == 0 {
		return text
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

type storedSession struct {
	data    []byte
	expires time.Time
}

// Store retains only AES-GCM ciphertext. Production derives its key from the
// shared deployment secret; tests may use an ephemeral random key.
type Store struct {
	mu       sync.Mutex
	aead     cipher.AEAD
	entries  map[string]storedSession
	ttl      time.Duration
	capacity int
	now      func() time.Time
}

func NewStore(capacity int, ttl time.Duration) (*Store, error) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, errors.New("privacy key unavailable")
	}
	return newStore(capacity, ttl, key[:])
}

func NewStoreWithSecret(capacity int, ttl time.Duration, secret string) (*Store, error) {
	if len(secret) < 32 {
		return nil, errors.New("privacy requires a configured secret of at least 32 bytes")
	}
	key := sha256.Sum256([]byte("new-api/prompt-privacy/v1:" + secret))
	return newStore(capacity, ttl, key[:])
}

func newStore(capacity int, ttl time.Duration, key []byte) (*Store, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Store{aead: aead, entries: make(map[string]storedSession), ttl: ttl, capacity: capacity, now: time.Now}, nil
}

func (s *Store) Open(owner, id string) (*Session, string, error) {
	if s == nil {
		return nil, "", errors.New("privacy store unavailable")
	}
	if id == "" {
		id, err := randomID()
		if err != nil {
			return nil, "", err
		}
		return NewSession(), id, nil
	}
	if len(id) != 32 {
		return nil, "", ErrSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.load(owner + ":" + id)
	return session, id, err
}

func (s *Store) load(key string) (*Session, error) {
	entry, ok := s.entries[key]
	if common.RedisEnabled && common.RDB != nil {
		value, err := common.RDB.Get(common.RDB.Context(), s.redisKey(key)).Bytes()
		if err == nil {
			entry, ok = storedSession{data: value, expires: s.now().Add(s.ttl)}, true
		} else if !errors.Is(err, redis.Nil) {
			return nil, errors.New("privacy session storage unavailable")
		} else {
			ok = false
		}
	}
	if !ok || !entry.expires.After(s.now()) {
		delete(s.entries, key)
		return nil, ErrSession
	}
	n := s.aead.NonceSize()
	if len(entry.data) < n {
		return nil, ErrSession
	}
	plain, err := s.aead.Open(nil, entry.data[:n], entry.data[n:], []byte(key))
	if err != nil {
		return nil, ErrSession
	}
	defer clear(plain)
	session := NewSession()
	if common.Unmarshal(plain, &session.Values) != nil {
		return nil, ErrSession
	}
	return session, nil
}

func (s *Store) Save(owner, id string, session *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + ":" + id
	values := session.snapshot()
	// Parallel turns merge their references instead of dropping the other turn.
	if old, err := s.load(key); err == nil {
		for ref, value := range old.Values {
			values[ref] = value
		}
		old.Clear()
	}
	if len(values) > MaxSecrets {
		return ErrLimit
	}
	for k, entry := range s.entries {
		if !entry.expires.After(s.now()) {
			delete(s.entries, k)
		}
	}
	// Most callers never send the session header back, so every request leaves
	// an entry behind. A full store drops the entry nearest to expiry; refusing
	// the new one would fail every request until the oldest expires.
	if _, exists := s.entries[key]; !exists && len(s.entries) >= s.capacity {
		oldest := ""
		for k, entry := range s.entries {
			if oldest == "" || entry.expires.Before(s.entries[oldest].expires) {
				oldest = k
			}
		}
		delete(s.entries, oldest)
	}
	plain, err := common.Marshal(values)
	if err != nil {
		return errors.New("privacy session encoding failed")
	}
	defer clear(plain)
	if len(plain) > 256<<10 {
		return ErrLimit
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return errors.New("privacy randomness unavailable")
	}
	s.entries[key] = storedSession{data: s.aead.Seal(nonce, nonce, plain, []byte(key)), expires: s.now().Add(s.ttl)}
	if common.RedisEnabled && common.RDB != nil {
		if err := common.RDB.Set(common.RDB.Context(), s.redisKey(key), s.entries[key].data, s.ttl).Err(); err != nil {
			delete(s.entries, key)
			return errors.New("privacy session storage unavailable")
		}
		delete(s.entries, key)
	}
	return nil
}

func (s *Store) redisKey(key string) string {
	hash := sha256.Sum256([]byte(key))
	return "privacy:prompt:v1:" + hex.EncodeToString(hash[:])
}
