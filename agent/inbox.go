package main

import (
	"sync"
)

type Inbox struct {
	items []InboxItem
	max   int
	mu    sync.RWMutex
}

func NewInbox(max int) *Inbox {
	return &Inbox{
		items: make([]InboxItem, 0),
		max:   max,
	}
}

func (in *Inbox) Add(item InboxItem) {
	in.mu.Lock()
	defer in.mu.Unlock()

	in.items = append(in.items, item)
	if len(in.items) > in.max {
		in.items = in.items[len(in.items)-in.max:]
	}
}

func (in *Inbox) GetAll() []InboxItem {
	in.mu.RLock()
	defer in.mu.RUnlock()

	result := make([]InboxItem, len(in.items))
	copy(result, in.items)
	return result
}

func (in *Inbox) Get(itemID string) *InboxItem {
	in.mu.RLock()
	defer in.mu.RUnlock()

	for i := range in.items {
		if in.items[i].ItemID == itemID {
			item := in.items[i]
			return &item
		}
	}
	return nil
}

func (in *Inbox) Remove(itemID string) {
	in.mu.Lock()
	defer in.mu.Unlock()

	for i := range in.items {
		if in.items[i].ItemID == itemID {
			in.items = append(in.items[:i], in.items[i+1:]...)
			return
		}
	}
}
