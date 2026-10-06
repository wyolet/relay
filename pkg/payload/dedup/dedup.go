// Package dedup splits a JSON request body into a skeleton and
// content-addressed pieces, and rebuilds the exact bytes from them. Agent
// clients resend the whole conversation every turn, so storing each unique
// message, tool definition and system prompt once removes most of a payload
// store's volume.
//
// Split works on raw bytes and never re-serialises JSON: a piece is the
// verbatim bytes of one array element (or one whole value), and the skeleton
// is the body with those bytes cut out. Rebuild must reproduce the input
// byte for byte; Split checks that itself and falls back to storing the body
// as a single piece when it cannot, so a caller never stores something it
// cannot read back.
//
// The split rule knows no wire shape: every top-level array is split per
// element, every other top-level value of at least minValuePiece bytes is
// one piece, and the rest stays in the skeleton.
//
// Out of scope: storage.
package dedup

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
)

// minValuePiece is the smallest non-array top-level value stored as its own
// piece; shorter values (model, flags, limits) cost less inline than a hash.
const minValuePiece = 1024

// Hash is the first 16 bytes of the SHA-256 of a piece's bytes. Pieces may
// be shared across owners, so the hash must resist construction: an
// accidental collision needs ~2^64 pieces, and corrupting another caller's
// piece needs a second preimage (~2^128 work).
type Hash [16]byte

func hashOf(b []byte) Hash {
	sum := sha256.Sum256(b)
	return Hash(sum[:len(Hash{})])
}

// Piece is one unique content-addressed fragment of a body. Body aliases
// the input passed to Split.
type Piece struct {
	Hash Hash
	Body []byte
}

// Field is where one top-level field's pieces go back into the skeleton.
type Field struct {
	// Name is the top-level key, empty for a body stored whole.
	Name string
	// Array is true when the value was an array split per element; the
	// pieces are rejoined with commas. Otherwise the value is one piece.
	Array bool
	// Offset is the byte position in the skeleton where the pieces go.
	Offset int
	// Hashes lists the field's pieces in order, repeats included.
	Hashes []Hash
}

// Split is a body broken into a skeleton plus pieces.
type Split struct {
	Skeleton []byte
	// Fields are in ascending Offset order.
	Fields []Field
	// Pieces holds each distinct piece once, in first-seen order.
	Pieces []Piece
}

// Whole reports whether the body was stored as a single piece.
func (s Split) Whole() bool {
	return len(s.Fields) == 1 && s.Fields[0].Name == ""
}

// SplitBody splits body. A body that is not a JSON object (truncated,
// binary, an array) or that would not rebuild exactly is returned as one
// piece.
func SplitBody(body []byte) Split {
	s, ok := split(body)
	if !ok {
		return whole(body)
	}
	got, err := Rebuild(s.Skeleton, s.Fields, s.lookup())
	if err != nil || !bytes.Equal(got, body) {
		return whole(body)
	}
	return s
}

func whole(body []byte) Split {
	h := hashOf(body)
	return Split{
		Fields: []Field{{Offset: 0, Hashes: []Hash{h}}},
		Pieces: []Piece{{Hash: h, Body: body}},
	}
}

func (s Split) lookup() func(Hash) ([]byte, bool) {
	m := make(map[Hash][]byte, len(s.Pieces))
	for _, p := range s.Pieces {
		m[p.Hash] = p.Body
	}
	return func(h Hash) ([]byte, bool) {
		b, ok := m[h]
		return b, ok
	}
}

func split(body []byte) (Split, bool) {
	members, ok := topLevelMembers(body)
	if !ok {
		return Split{}, false
	}

	var s Split
	seen := map[Hash]bool{}
	add := func(b []byte) Hash {
		h := hashOf(b)
		if !seen[h] {
			seen[h] = true
			s.Pieces = append(s.Pieces, Piece{Hash: h, Body: b})
		}
		return h
	}

	skeleton := make([]byte, 0, 256)
	prev := 0
	for _, m := range members {
		isArray := body[m.valueStart] == '['
		if !isArray && m.valueEnd-m.valueStart < minValuePiece {
			continue
		}
		f := Field{Name: m.key, Array: isArray}
		if isArray {
			elems, ok := arrayElements(body, m.valueStart, m.valueEnd)
			if !ok {
				return Split{}, false
			}
			// The skeleton keeps the brackets and any whitespace around the
			// elements; Rebuild rejoins them with bare commas, so any other
			// separator is not splittable.
			start := m.valueStart + 1
			if len(elems) > 0 {
				start = elems[0][0]
			}
			skeleton = append(skeleton, body[prev:start]...)
			f.Offset = len(skeleton)
			cut := start
			for i, e := range elems {
				if i > 0 && !bytes.Equal(body[cut:e[0]], comma) {
					return Split{}, false
				}
				f.Hashes = append(f.Hashes, add(body[e[0]:e[1]]))
				cut = e[1]
			}
			prev = cut
		} else {
			skeleton = append(skeleton, body[prev:m.valueStart]...)
			f.Offset = len(skeleton)
			f.Hashes = []Hash{add(body[m.valueStart:m.valueEnd])}
			prev = m.valueEnd
		}
		s.Fields = append(s.Fields, f)
	}
	skeleton = append(skeleton, body[prev:]...)
	s.Skeleton = skeleton
	return s, true
}

var comma = []byte{','}

// ErrMissingPiece is returned by Rebuild when lookup has no piece for a hash.
var ErrMissingPiece = errors.New("dedup: missing piece")

// Rebuild reassembles a body from its skeleton, fields and a piece lookup.
func Rebuild(skeleton []byte, fields []Field, lookup func(Hash) ([]byte, bool)) ([]byte, error) {
	var out bytes.Buffer
	prev := 0
	for _, f := range fields {
		if f.Offset < prev || f.Offset > len(skeleton) {
			return nil, fmt.Errorf("dedup: field %q offset %d out of order", f.Name, f.Offset)
		}
		out.Write(skeleton[prev:f.Offset])
		for i, h := range f.Hashes {
			b, ok := lookup(h)
			if !ok {
				return nil, fmt.Errorf("%w: %x", ErrMissingPiece, h)
			}
			if i > 0 && f.Array {
				out.WriteByte(',')
			}
			out.Write(b)
		}
		prev = f.Offset
	}
	out.Write(skeleton[prev:])
	return out.Bytes(), nil
}

// CommonPrefix returns how many leading hashes a and b share. For two
// requests of one conversation, the first differing index is where a
// prompt-cache prefix stops matching.
func CommonPrefix(a, b []Hash) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
