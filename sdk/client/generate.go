package client

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Generate runs a non-streaming generation. OutputMode is forced to sync; the
// caller's request is not mutated.
func (c *Client) Generate(ctx context.Context, req *v1.Request) (*Response, error) {
	if c.syncTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.syncTimeout)
		defer cancel()
	}
	resp, err := c.roundTrip(ctx, req, v1.OutputModeSync)
	if err != nil {
		return nil, err
	}
	defer resp.body.Close()

	body, err := io.ReadAll(resp.body)
	if err != nil {
		return nil, fmt.Errorf("%s: read body: %w", c.host(), err)
	}
	if resp.status/100 != 2 {
		return nil, parseAPIError(c.host(), resp.status, body)
	}
	wire, err := c.translator.ParseResponse(body)
	if err != nil {
		return nil, err
	}
	return c.wrapResponse(wire), nil
}

// GenerateStream runs a streaming generation. The returned *Stream yields
// canonical events until io.EOF; the caller must Close it. OutputMode is forced
// to stream.
func (c *Client) GenerateStream(ctx context.Context, req *v1.Request) (*Stream, error) {
	start := time.Now() // anchor for Timing() offsets, like relay's request-accept
	resp, err := c.roundTrip(ctx, req, v1.OutputModeStream)
	if err != nil {
		return nil, err
	}
	if resp.status/100 != 2 {
		body, _ := io.ReadAll(resp.body)
		_ = resp.body.Close()
		return nil, parseAPIError(c.host(), resp.status, body)
	}
	sc := bufio.NewScanner(resp.body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	sc.Split(splitSSEFrames)
	return c.wrapStream(&Stream{
		body:    resp.body,
		sc:      sc,
		toCanon: c.translator.NewToCanonicalStream(),
		start:   start,
	}), nil
}

func (c *Client) wrapResponse(r *v1.Response) *Response {
	out := &Response{Response: r}
	if len(c.target.binding.Pricing) > 0 {
		out.binding = c.target.binding
		out.priced = true
	}
	return out
}

func (c *Client) wrapStream(s *Stream) *Stream {
	if len(c.target.binding.Pricing) > 0 {
		s.binding = c.target.binding
		s.priced = true
	}
	return s
}

// roundTrip serializes the request (translator-owned) and hands the bytes
// to the transport. The caller's request is not mutated.
func (c *Client) roundTrip(ctx context.Context, req *v1.Request, mode string) (*rtResponse, error) {
	if c.configErr != nil {
		return nil, c.configErr
	}
	r := *req // shallow copy: don't mutate the caller's request
	r.OutputMode = mode
	if c.target.upstream != "" {
		if len(r.Model) == 0 {
			r.Model = v1.ModelRefs{c.target.upstream}
		} else {
			r.Model = append(v1.ModelRefs(nil), r.Model...)
			r.Model[0] = c.target.upstream
		}
	}
	body, err := c.translator.SerializeRequest(&r)
	if err != nil {
		return nil, fmt.Errorf("relay client: serialize request: %w", err)
	}
	path := c.path
	if c.pathFn != nil {
		var model string
		if c.target.upstream != "" {
			model = c.target.upstream
		} else if len(req.Model) > 0 {
			model = req.Model[0]
		}
		path = c.pathFn(model, mode == v1.OutputModeStream)
	}
	return c.transport.roundTrip(ctx, c, path, body)
}

// host returns the authority of the client's base URL, used to attribute
// errors to the upstream actually dialed. Falls back to the raw base URL
// when it doesn't parse.
func (c *Client) host() string {
	if u, err := url.Parse(c.baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return c.baseURL
}
