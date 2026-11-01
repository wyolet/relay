package engine_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/wyolet/relay/pkg/engine"
)

// BenchmarkLoadRowsSharingOneName loads n rows that all carry one name; ns/row stays flat as n grows when inserting a holder does not copy the others.
func BenchmarkLoadRowsSharingOneName(b *testing.B) {
	for _, n := range []int{1_000, 4_000, 16_000} {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			src := newMemSrc(func(p *parent) string { return p.ID })
			for i := range n {
				src.set(&parent{ID: fmt.Sprintf("p%06d", i), Name: "shared", Ver: 1})
			}
			for b.Loop() {
				e := engine.New(engine.Options{})
				engine.RegisterKind(e, engine.KindSpec[parent]{
					Kind:   kParent,
					Meta:   func(p *parent) engine.Meta { return engine.Meta{ID: p.ID, Name: p.Name, Version: p.Ver} },
					Source: src,
				})
				if err := e.Load(context.Background()); err != nil {
					b.Fatal(err)
				}
				_ = e.Close()
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/row")
		})
	}
}
