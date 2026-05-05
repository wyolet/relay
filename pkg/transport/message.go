// Package transport defines Message, the canonical primitive that
// flows through Relay.
package transport

// Message flows through the pipeline. Inbound adapters build it from
// the wire payload; outbound adapters consume it. Response is
// delivered back via Return.
type Message struct {
	ID     string
	Body   []byte
	Return chan<- *Message
}
