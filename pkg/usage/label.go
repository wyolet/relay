package usage

import "time"

// labelColumns pairs each id dimension with the event-time slug recorded beside it. credential_id pairs with relay_key because a key credential's id is the key's id, and token credentials carry no relay_key, so they stay unlabeled.
var labelColumns = map[string]string{
	"model_id":       "model",
	"host_id":        "host",
	"policy_id":      "policy",
	"project_id":     "project",
	"team_id":        "team",
	"principal_id":   "principal",
	"relay_key_hash": "relay_key",
	"credential_id":  "relay_key",
}

// LabelColumn returns the slug column that names values of the id dimension dim. ok is false for slug dimensions and ids without a slug (host_key_id). The column is itself a group dimension, so it is safe to embed in SQL.
func LabelColumn(dim string) (string, bool) {
	col, ok := labelColumns[dim]
	return col, ok
}

// LatestLabels keeps the newest non-empty label per non-empty group value. The zero value is ready to use.
type LatestLabels struct {
	byValue map[string]stampedLabel
}

type stampedLabel struct {
	label string
	ts    time.Time
}

// Add records label for value as seen at ts, unless a newer one is already held.
func (l *LatestLabels) Add(value, label string, ts time.Time) {
	if value == "" || label == "" {
		return
	}
	if l.byValue == nil {
		l.byValue = map[string]stampedLabel{}
	}
	if cur, ok := l.byValue[value]; !ok || !ts.Before(cur.ts) {
		l.byValue[value] = stampedLabel{label: label, ts: ts}
	}
}

// Get returns value's label, empty when none was recorded.
func (l *LatestLabels) Get(value string) string {
	return l.byValue[value].label
}

// Map returns value → label, nil when nothing was recorded.
func (l *LatestLabels) Map() map[string]string {
	if len(l.byValue) == 0 {
		return nil
	}
	out := make(map[string]string, len(l.byValue))
	for v, s := range l.byValue {
		out[v] = s.label
	}
	return out
}

// addEvent records ev's slug for value when dim has a label column; the in-memory engine's counterpart of the SQL latest-label aggregate.
func (l *LatestLabels) addEvent(dim, value string, ev Event) {
	if col, ok := LabelColumn(dim); ok {
		l.Add(value, groupKey(ev, col), ev.Timestamp)
	}
}
