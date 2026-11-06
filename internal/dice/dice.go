// Package dice provides randomness that replays.
//
// There is no stored RNG state. Every roll is derived from (seed, tick, stream),
// so a retried tick rolls the same numbers and a service restart can't fork the
// timeline. Adding a new stream also can't shift the rolls of existing ones,
// which is what makes balance changes reviewable.
package dice

import (
	"hash/fnv"
	"math/rand/v2"
)

func For(seed int64, tick int32, stream string) *rand.Rand {
	h := fnv.New64a()
	h.Write([]byte(stream))
	return rand.New(rand.NewPCG(uint64(seed)^h.Sum64(), uint64(tick)))
}
