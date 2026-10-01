package embed

import (
	"database/sql/driver"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Vector is a pgvector `vector` value. It works as a query argument and a scan target with pgx and database/sql without
// registering a type: pgx sends an unregistered type's argument as text, and the literal "[0.1,0.2]" is the text form pgvector
// reads and writes. (This package deliberately does not depend on github.com/pgvector/pgvector-go: see the shared docs.)
//
//	_, err := pool.Exec(ctx, `INSERT INTO notes (body, embedding) VALUES ($1, $2)`, body, embed.Vector(vec))
//	var v embed.Vector
//	err = pool.QueryRow(ctx, `SELECT embedding FROM notes WHERE id = $1`, id).Scan(&v)
//
// With sqlc, override the type: `vector` -> `github.com/teb-ooo/playground-go/embed.Vector` (see data-and-secrets.md).
type Vector []float32

// Literal formats v as a pgvector literal, "[0.1,0.2,0.3]". It does not check for NaN or infinity (Value does).
func Literal(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*10 + 2)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// ParseLiteral parses "[0.1,0.2]" (the text pgvector returns).
func ParseLiteral(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("embed: %q is not a vector literal", s)
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	if body == "" {
		return []float32{}, nil
	}
	parts := strings.Split(body, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("embed: vector element %d: %w", i, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}

// Value implements driver.Valuer. NaN and infinity are refused, as pgvector refuses them.
func (v Vector) Value() (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	for i, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, fmt.Errorf("embed: vector element %d is %v", i, x)
		}
	}
	return Literal(v), nil
}

// Scan implements sql.Scanner for the text form.
func (v *Vector) Scan(src any) error {
	switch s := src.(type) {
	case nil:
		*v = nil
		return nil
	case string:
		out, err := ParseLiteral(s)
		*v = out
		return err
	case []byte:
		out, err := ParseLiteral(string(s))
		*v = out
		return err
	}
	return fmt.Errorf("embed: cannot scan %T into a Vector", src)
}
