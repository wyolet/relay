package file

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

type countingBuffer struct {
	bytes.Buffer
	writes int
}

func (c *countingBuffer) Write(p []byte) (int, error) {
	c.writes++
	if !bytes.HasSuffix(p, []byte("\n")) {
		return 0, fmt.Errorf("write of %d bytes does not end on a line", len(p))
	}
	return c.Buffer.Write(p)
}

func TestSink_WriteBatchMatchesWrite(t *testing.T) {
	events := make([]usage.Event, 400)
	for i := range events {
		events[i] = usage.Event{
			RequestID:    fmt.Sprintf("r%d", i),
			Source:       "pipeline",
			Timestamp:    time.Unix(1_700_000_000, int64(i)).UTC(),
			Status:       200,
			ErrorMessage: strings.Repeat("<e>", 100),
		}
	}

	var perEvent bytes.Buffer
	one := NewSinkFromWriter(&perEvent)
	for _, ev := range events {
		if err := one.Write(ev); err != nil {
			t.Fatal(err)
		}
	}

	var batched countingBuffer
	if err := NewSinkFromWriter(&batched).WriteBatch(events); err != nil {
		t.Fatal(err)
	}

	if batched.String() != perEvent.String() {
		t.Fatal("WriteBatch output differs from per-event Write output")
	}
	// ~150 KiB of lines: a few whole-line chunks, not one write per event.
	if batched.writes < 2 || batched.writes > 4 {
		t.Fatalf("WriteBatch made %d writes for %d bytes, want chunks of about %d bytes", batched.writes, batched.Len(), batchWriteBytes)
	}
}
