package embed

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strings"
	"unicode"
)

// Fake is an Embedder for tests: no service, deterministic, and the right size. A text's vector is the normalised sum of
// one pseudo-random vector per lower-cased word, so equal texts are identical (cosine 1), texts that share words are closer
// than texts that share none, and the same input always gives the same output. Kind does not change the vector, so a
// query equal to a stored passage finds it. It carries no meaning beyond shared words; do not judge relevance with it.
type Fake struct {
	// Dims is the vector size; 0 means DefaultDimensions.
	Dims int
}

// Dimensions implements Embedder.
func (f Fake) Dimensions() int {
	if f.Dims <= 0 {
		return DefaultDimensions
	}
	return f.Dims
}

// Embed implements Embedder.
func (f Fake) Embed(ctx context.Context, texts []string, _ Kind) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = f.vector(t)
	}
	return out, nil
}

func (f Fake) vector(text string) []float32 {
	n := f.Dimensions()
	sum := make([]float64, n)
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(words) == 0 {
		words = []string{""}
	}
	for _, w := range words {
		var counter uint32
		var block [32]byte
		pos := len(block)
		for d := 0; d < n; d++ {
			if pos+4 > len(block) {
				var in [4]byte
				binary.BigEndian.PutUint32(in[:], counter)
				block = sha256.Sum256(append([]byte(w), in[:]...))
				counter++
				pos = 0
			}
			u := binary.BigEndian.Uint32(block[pos : pos+4])
			pos += 4
			sum[d] += float64(u)/float64(math.MaxUint32)*2 - 1
		}
	}
	var norm float64
	for _, x := range sum {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	v := make([]float32, n)
	if norm == 0 {
		v[0] = 1
		return v
	}
	for i, x := range sum {
		v[i] = float32(x / norm)
	}
	return v
}
