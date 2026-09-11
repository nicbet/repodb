package storage

import (
	"context"
	"sync"
)

type Memory struct {
	mu      sync.RWMutex
	objects map[Hash][]byte
}

func NewMemory() *Memory {
	return &Memory{objects: make(map[Hash][]byte)}
}

func (m *Memory) Get(_ context.Context, hash Hash) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.objects[hash]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (m *Memory) Put(_ context.Context, data []byte) (Hash, error) {
	hash := Sum(data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[hash]; !ok {
		m.objects[hash] = append([]byte(nil), data...)
	}
	return hash, nil
}
