package file

import (
	"testing"

	"github.com/wyolet/relay/pkg/usage"
	"github.com/wyolet/relay/pkg/usage/usagetest"
)

func TestReaderConformance(t *testing.T) {
	usagetest.Run(t, func(t *testing.T, events []usage.Event) usage.Reader {
		return NewReader(writeFixture(t, events))
	})
}
