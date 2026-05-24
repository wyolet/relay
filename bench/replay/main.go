// Command replay exercises Relay's /v1/messages endpoint against every turn
// from a captured Claude Code session JSONL. It reconstructs the realistic
// outbound request body that Claude Code would have sent at each assistant
// turn (full conversation history up to that point), fires them serially
// through Relay, then reports overhead p50/p99 harvested from Prometheus.
//
// Serial mode only — p50/p99 reflect sequential latency, not under-load
// concurrency. Use bench/bench_test.go for load numbers.
//
// Usage:
//
//	go run ./bench/replay --session <path> [--relay http://localhost:8080] [--key test-key]
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sessionLine is a single line from the Claude Code JSONL.
type sessionLine struct {
	Type       string          `json:"type"`
	UUID       string          `json:"uuid"`
	ParentUUID *string         `json:"parentUuid"`
	Message    json.RawMessage `json:"message"`
}

// msgContent is the Anthropic message shape stored in assistant lines.
type msgContent struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Role    string            `json:"role"`
	Model   string            `json:"model"`
	Content []json.RawMessage `json:"content"`
}

// userMsg is the shape stored in user lines.
type userMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []block
}

// turn is a resolved conversation node.
type turn struct {
	role    string // "user" or "assistant"
	content json.RawMessage
}

// request is the body we POST to /v1/messages.
type request struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	Messages  []map[string]any  `json:"messages"`
	Stream    bool              `json:"stream,omitempty"`
}

func main() {
	session := flag.String("session", defaultSession(), "path to Claude Code session JSONL")
	relay := flag.String("relay", "http://localhost:8080", "relay base URL")
	key := flag.String("key", "test-key", "customer API key")
	// Override model sent to relay. The captured session uses "claude-opus-4-7" but
	// the fake-anthropic provider is registered as "fake-claude" in the default seed.
	// Pass "" to use the model name from the session verbatim.
	model := flag.String("model", "fake-claude", "override model name (default: fake-claude)")
	// metricsURLs is a comma-separated list of /metrics endpoints to aggregate.
	// Default picks up both relay pods directly to avoid nginx split-brain.
	metricsFlag := flag.String("metrics", "http://localhost:5101/metrics,http://localhost:5102/metrics",
		"comma-separated Prometheus /metrics URLs (aggregated for histogram)")
	flag.Parse()
	metricsURLs := strings.Split(*metricsFlag, ",")

	nodes, err := loadSession(*session)
	if err != nil {
		log.Fatalf("load session: %v", err)
	}

	// Build uuid→node map and adjacency.
	byUUID := make(map[string]*sessionLine, len(nodes))
	for i := range nodes {
		byUUID[nodes[i].UUID] = &nodes[i]
	}

	// Extract ordered assistant turns from file order.
	// For each assistant turn, walk its parent chain to reconstruct
	// the conversation context (all preceding user+assistant turns).
	type replayTurn struct {
		idx      int
		model    string
		messages []map[string]any
	}

	var turns []replayTurn
	seenUUID := make(map[string]bool)

	for i := range nodes {
		node := &nodes[i]
		if node.Type != "assistant" || seenUUID[node.UUID] {
			continue
		}
		seenUUID[node.UUID] = true

		// Decode assistant message to get model.
		var am msgContent
		if err := json.Unmarshal(node.Message, &am); err != nil || am.Model == "" {
			continue
		}

		// Walk parent chain (excluding this node) to build history.
		history := walkHistory(node.ParentUUID, byUUID, 200)

		if len(history) == 0 {
			// No context — skip; bare assistant turns without a user prefix
			// cannot form a valid request.
			continue
		}

		// Convert history to messages array.
		msgs := make([]map[string]any, 0, len(history))
		for _, h := range history {
			var raw json.RawMessage
			if h.role == "user" {
				var um userMsg
				if err := json.Unmarshal(h.content, &um); err != nil {
					continue
				}
				raw = um.Content
			} else {
				var am2 msgContent
				if err := json.Unmarshal(h.content, &am2); err != nil {
					continue
				}
				if len(am2.Content) == 0 {
					continue
				}
				b, _ := json.Marshal(am2.Content)
				raw = b
			}
			msgs = append(msgs, map[string]any{
				"role":    h.role,
				"content": raw,
			})
		}

		if len(msgs) == 0 || msgs[0]["role"] != "user" {
			continue
		}

		m := am.Model
		if *model != "" {
			m = *model
		}
		turns = append(turns, replayTurn{
			idx:      len(turns),
			model:    m,
			messages: msgs,
		})
	}

	log.Printf("session %s: %d replay turns", filepath.Base(*session), len(turns))

	// Snapshot Prometheus metrics before replay (aggregate across all pods).
	beforeCount, beforeSum, beforeBuckets := scrapeOverheadAll(metricsURLs)

	var (
		ok        int
		fail      int
		streaming int
		wallTimes []time.Duration
		firstFail string
	)

	client := &http.Client{Timeout: 120 * time.Second}

	for i, t := range turns {
		useStream := i%4 == 3 // 25% streaming turns

		body := request{
			Model:    t.model,
			MaxTokens: 4096, // JSONL doesn't capture original max_tokens; 4096 is Claude Code's default
			Messages:  t.messages,
			Stream:    useStream,
		}
		bodyBytes, _ := json.Marshal(body)

		req, _ := http.NewRequest("POST", *relay+"/anthropic/v1/messages", strings.NewReader(string(bodyBytes)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", *key)

		start := time.Now()
		resp, err := client.Do(req)
		elapsed := time.Since(start)

		if err != nil {
			fail++
			if firstFail == "" {
				firstFail = fmt.Sprintf("turn %d net error: %v", i, err)
			}
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			fail++
			if firstFail == "" {
				trunc := respBody
				if len(trunc) > 200 {
					trunc = trunc[:200]
				}
				firstFail = fmt.Sprintf("turn %d status %d body %s", i, resp.StatusCode, trunc)
			}
			continue
		}

		if useStream {
			if err := validateSSE(respBody); err != nil {
				fail++
				if firstFail == "" {
					firstFail = fmt.Sprintf("turn %d sse error: %v", i, err)
				}
				continue
			}
			streaming++
		} else {
			if err := validateJSON(respBody); err != nil {
				fail++
				if firstFail == "" {
					firstFail = fmt.Sprintf("turn %d json error: %v", i, err)
				}
				continue
			}
		}

		ok++
		wallTimes = append(wallTimes, elapsed)
	}

	// Harvest metrics after replay.
	afterCount, afterSum, afterBuckets := scrapeOverheadAll(metricsURLs)

	deltaCount := afterCount - beforeCount
	deltaSum := afterSum - beforeSum

	var avgOverhead float64
	if deltaCount > 0 {
		avgOverhead = deltaSum / float64(deltaCount) * 1000
	}

	p50overhead, p99overhead := quantilesFromBuckets(beforeBuckets, afterBuckets, deltaCount)
	wallP50, wallP99 := wallQuantiles(wallTimes)

	fmt.Printf("replay %s\n", *session)
	fmt.Printf("turns: %d  ok: %d  fail: %d  streaming: %d\n", len(turns), ok, fail, streaming)
	fmt.Printf("overhead p50: %.2fms  p99: %.2fms  avg: %.2fms  count: %d\n",
		p50overhead, p99overhead, avgOverhead, int(deltaCount))
	fmt.Printf("wall-clock per req: avg %.2fms  p99 %.2fms\n", wallP50, wallP99)
	if firstFail != "" {
		fmt.Printf("first failure: %s\n", firstFail)
	}
}

// walkHistory walks the parent chain from parentUUID, returning turns in
// chronological order (oldest first), capped at maxDepth nodes.
func walkHistory(parentUUID *string, byUUID map[string]*sessionLine, maxDepth int) []turn {
	if parentUUID == nil {
		return nil
	}
	var chain []turn
	cur := *parentUUID
	seen := make(map[string]bool)
	for cur != "" && !seen[cur] && len(chain) < maxDepth {
		seen[cur] = true
		node, ok := byUUID[cur]
		if !ok {
			break
		}
		if node.Type == "user" || node.Type == "assistant" {
			chain = append(chain, turn{role: node.Type, content: node.Message})
		}
		if node.ParentUUID == nil {
			break
		}
		cur = *node.ParentUUID
	}
	// Reverse to chronological.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

func loadSession(path string) ([]sessionLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []sessionLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for sc.Scan() {
		var sl sessionLine
		if err := json.Unmarshal(sc.Bytes(), &sl); err != nil || sl.UUID == "" {
			continue
		}
		if sl.Type != "user" && sl.Type != "assistant" {
			continue
		}
		out = append(out, sl)
	}
	return out, sc.Err()
}

func validateJSON(body []byte) error {
	var m struct {
		ID      string            `json:"id"`
		Type    string            `json:"type"`
		Content []json.RawMessage `json:"content"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return err
	}
	if m.ID == "" {
		return fmt.Errorf("missing id")
	}
	if m.Type != "message" {
		return fmt.Errorf("unexpected type %q", m.Type)
	}
	return nil
}

func validateSSE(body []byte) error {
	lines := strings.Split(string(body), "\n")
	var events []string
	for _, l := range lines {
		if strings.HasPrefix(l, "event: ") {
			events = append(events, strings.TrimPrefix(l, "event: "))
		}
	}
	if len(events) == 0 {
		return fmt.Errorf("no SSE events")
	}
	if events[0] != "message_start" {
		return fmt.Errorf("first event %q, want message_start", events[0])
	}
	if events[len(events)-1] != "message_stop" {
		return fmt.Errorf("last event %q, want message_stop", events[len(events)-1])
	}
	return nil
}

// scrapeOverheadAll aggregates scrapeOverhead across multiple URLs (multi-pod).
func scrapeOverheadAll(urls []string) (count float64, sum float64, buckets map[float64]float64) {
	buckets = make(map[float64]float64)
	for _, u := range urls {
		c, s, b := scrapeOverhead(strings.TrimSpace(u))
		count += c
		sum += s
		for le, v := range b {
			buckets[le] += v
		}
	}
	return
}

// scrapeOverhead fetches /metrics and extracts relay_anthropic_messages_overhead_seconds
// count, sum, and bucket values (le → cumulative count).
func scrapeOverhead(url string) (count float64, sum float64, buckets map[float64]float64) {
	buckets = make(map[float64]float64)
	resp, err := http.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		const prefix = "relay_anthropic_messages_overhead_seconds"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := line[len(prefix):]
		val := parseMetricValue(line)
		if strings.HasPrefix(rest, "_count") {
			count = val
		} else if strings.HasPrefix(rest, "_sum") {
			sum = val
		} else if strings.HasPrefix(rest, "_bucket{") {
			le := extractLE(rest)
			buckets[le] = val
		}
	}
	return
}

func parseMetricValue(line string) float64 {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0
	}
	v, _ := strconv.ParseFloat(parts[len(parts)-1], 64)
	return v
}

func extractLE(s string) float64 {
	// s looks like: _bucket{le="0.001"} 42
	start := strings.Index(s, `le="`)
	if start < 0 {
		return math.Inf(1)
	}
	start += 4
	end := strings.Index(s[start:], `"`)
	if end < 0 {
		return math.Inf(1)
	}
	leStr := s[start : start+end]
	if leStr == "+Inf" {
		return math.Inf(1)
	}
	v, _ := strconv.ParseFloat(leStr, 64)
	return v
}

type bucket struct {
	le    float64
	count float64
}

// quantilesFromBuckets computes p50 and p99 in ms from Prometheus histogram deltas.
func quantilesFromBuckets(before, after map[float64]float64, totalCount float64) (p50ms, p99ms float64) {
	if totalCount == 0 {
		return 0, 0
	}

	var bkts []bucket
	for le, v := range after {
		delta := v - before[le]
		bkts = append(bkts, bucket{le, delta})
	}
	sort.Slice(bkts, func(i, j int) bool {
		return bkts[i].le < bkts[j].le
	})

	p50ms = quantileFromBuckets(bkts, totalCount, 0.50) * 1000
	p99ms = quantileFromBuckets(bkts, totalCount, 0.99) * 1000
	return
}

func quantileFromBuckets(bkts []bucket, total, q float64) float64 {
	target := q * total
	var prev float64
	var prevLE float64
	for _, b := range bkts {
		if b.count >= target {
			// Linear interpolation within bucket.
			if b.count == prev {
				return b.le
			}
			frac := (target - prev) / (b.count - prev)
			return prevLE + frac*(b.le-prevLE)
		}
		prev = b.count
		prevLE = b.le
	}
	return prevLE
}

func wallQuantiles(times []time.Duration) (avgMs, p99Ms float64) {
	if len(times) == 0 {
		return 0, 0
	}
	sorted := make([]float64, len(times))
	var sum float64
	for i, t := range times {
		ms := float64(t.Milliseconds())
		sorted[i] = ms
		sum += ms
	}
	sort.Float64s(sorted)
	avgMs = sum / float64(len(sorted))
	p99Ms = sorted[int(float64(len(sorted))*0.99)]
	return
}

func defaultSession() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects",
		"-Users-abror-projects-wyolet-workspace",
		"b403f9fd-0f2b-4e44-925d-162befdd7b14.jsonl")
}
