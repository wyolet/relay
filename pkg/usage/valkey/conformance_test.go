package valkey_test

import (
	"testing"

	"github.com/wyolet/relay/pkg/usage"
	"github.com/wyolet/relay/pkg/usage/usagetest"
)

func TestConformance(t *testing.T) {
	usagetest.Run(t, func(t *testing.T, events []usage.Event) usage.Reader {
		sk := newSink(t)
		for _, ev := range events {
			if err := sk.Write(ev); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		return sk
	})
}
