package types

import (
	"sync"

	"github.com/QuantumNous/new-api/common"
)

type RWMap[K comparable, V any] struct {
	data  map[K]V
	mutex sync.RWMutex
}

func (m *RWMap[K, V]) UnmarshalJSON(b []byte) error {
	// Parse into a fresh map first so invalid JSON leaves the live data intact.
	data := make(map[K]V)
	if err := common.Unmarshal(b, &data); err != nil {
		return err
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.data = data
	return nil
}

func (m *RWMap[K, V]) MarshalJSON() ([]byte, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return common.Marshal(m.data)
}

func NewRWMap[K comparable, V any]() *RWMap[K, V] {
	return &RWMap[K, V]{
		data: make(map[K]V),
	}
}

func (m *RWMap[K, V]) Get(key K) (V, bool) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	value, exists := m.data[key]
	return value, exists
}

func (m *RWMap[K, V]) Set(key K, value V) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.data[key] = value
}

func (m *RWMap[K, V]) AddAll(other map[K]V) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	for k, v := range other {
		m.data[k] = v
	}
}

func (m *RWMap[K, V]) Clear() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.data = make(map[K]V)
}

// ReadAll returns a copy of the entire map.
func (m *RWMap[K, V]) ReadAll() map[K]V {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	copiedMap := make(map[K]V)
	for k, v := range m.data {
		copiedMap[k] = v
	}
	return copiedMap
}

func (m *RWMap[K, V]) Len() int {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return len(m.data)
}

func LoadFromJsonString[K comparable, V any](m *RWMap[K, V], jsonStr string) error {
	return LoadFromJsonStringWithCallback(m, jsonStr, nil)
}

// LoadFromJsonStringWithCallback loads a JSON string into the RWMap and calls the callback on success.
// The live map is swapped only after a successful parse: these maps back billing
// ratios that are hot-reloaded from admin input, so a malformed value must never
// empty them.
func LoadFromJsonStringWithCallback[K comparable, V any](m *RWMap[K, V], jsonStr string, onSuccess func()) error {
	data := make(map[K]V)
	if err := common.Unmarshal([]byte(jsonStr), &data); err != nil {
		return err
	}
	m.mutex.Lock()
	m.data = data
	m.mutex.Unlock()
	if onSuccess != nil {
		onSuccess()
	}
	return nil
}

// MarshalJSONString returns the JSON string representation of the RWMap.
func (m *RWMap[K, V]) MarshalJSONString() string {
	bytes, err := m.MarshalJSON()
	if err != nil {
		return "{}"
	}
	return string(bytes)
}
