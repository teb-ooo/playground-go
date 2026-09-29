// Package uuidv7 generates RFC 9562 version 7 UUIDs, the ID format used by
// every factory app (BOOTSTRAP section 7).
package uuidv7

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

var (
	mu       sync.Mutex
	lastMs   int64
	lastRand uint16 // 12-bit counter keeps IDs monotonic within one millisecond
)

// New returns a new UUIDv7 in canonical string form. It panics only if the
// system random source fails, which is not recoverable.
func New() string {
	return format(time.Now(), nil)
}

func format(now time.Time, override *[10]byte) string {
	var b [16]byte
	ms := now.UnixMilli()

	mu.Lock()
	if ms <= lastMs {
		ms = lastMs
		lastRand++
		if lastRand > 0x0fff {
			ms++
			lastRand = 0
		}
	} else {
		lastRand = 0
	}
	lastMs = ms
	counter := lastRand
	mu.Unlock()

	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(ms))
	copy(b[0:6], ts[2:])

	var r [10]byte
	if override != nil {
		r = *override
	} else if _, err := rand.Read(r[:]); err != nil {
		panic(fmt.Sprintf("uuidv7: random source failed: %v", err))
	}
	b[6] = 0x70 | byte(counter>>8)&0x0f
	b[7] = byte(counter)
	b[8] = 0x80 | r[0]&0x3f
	copy(b[9:], r[1:8])

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Valid reports whether s is a canonical lower or upper case UUID string of
// version 7.
func Valid(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return s[14] == '7'
}
