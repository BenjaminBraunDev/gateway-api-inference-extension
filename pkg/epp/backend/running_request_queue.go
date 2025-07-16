package backend

import (
	"container/heap"
	"fmt"
)

// Request represents an element in the priority queue.
// The index is needed by heap.Remove and is maintained by the heap.Interface methods.
type Request struct {
	ID    string  // Unique identifier
	TPOT  float64 // The priority value (lower is higher priority)
	index int
}

// RequestPriorityQueue implements a priority queue with item removal by ID.
type RequestPriorityQueue struct {
	items  []*Request
	lookup map[string]*Request
}

// NewRequestPriorityQueue initializes and returns a new PriorityQueue.
func NewRequestPriorityQueue() *RequestPriorityQueue {
	return &RequestPriorityQueue{
		lookup: make(map[string]*Request),
	}
}

// Len is the number of items in the queue.
func (pq *RequestPriorityQueue) Len() int { return len(pq.items) }

// Less reports whether the item with index i should sort before the item with index j.
func (pq *RequestPriorityQueue) Less(i, j int) bool {
	return pq.items[i].TPOT < pq.items[j].TPOT
}

// Swap swaps the items with indexes i and j.
func (pq *RequestPriorityQueue) Swap(i, j int) {
	pq.items[i], pq.items[j] = pq.items[j], pq.items[i]
	pq.items[i].index = i
	pq.items[j].index = j
}

// Push adds an item to the heap.
func (pq *RequestPriorityQueue) Push(x any) {
	item := x.(*Request)
	item.index = len(pq.items)
	pq.items = append(pq.items, item)
}

// Pop removes and returns the minimum item from the heap.
func (pq *RequestPriorityQueue) Pop() any {
	n := len(pq.items)
	item := pq.items[n-1]
	pq.items[n-1] = nil // avoid memory leak
	item.index = -1     // for safety
	pq.items = pq.items[0 : n-1]
	return item
}

// Add adds a new item to the queue.
func (pq *RequestPriorityQueue) Add(id string, tpot float64) {
	// If item already exists, do not add. Use an Update method if needed.
	if _, exists := pq.lookup[id]; exists {
		return
	}
	item := &Request{
		ID:   id,
		TPOT: tpot,
	}
	pq.lookup[id] = item
	heap.Push(pq, item)
}

// Remove removes an item from the queue by its ID.
func (pq *RequestPriorityQueue) Remove(id string) (*Request, bool) {
	item, ok := pq.lookup[id]
	if !ok {
		return nil, false
	}
	removed := heap.Remove(pq, item.index).(*Request)
	delete(pq.lookup, id)
	return removed, true
}

// Peek returns the item with the lowest value without removing it.
func (pq *RequestPriorityQueue) Peek() *Request {
	if len(pq.items) == 0 {
		return nil
	}
	return pq.items[0]
}

// String returns a string representation of the queue for debugging.
func (pq *RequestPriorityQueue) String() string {
	result := "RequestPriorityQueue: ["
	for _, item := range pq.items {
		result += item.ID + "(" + fmt.Sprintf("%.2f", item.TPOT) + "), "
	}
	if len(result) > 1 {
		result = result[:len(result)-2] // remove trailing comma and space
	}
	result += "]"
	return result
}
