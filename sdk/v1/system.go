package v1

import "strings"

// System-marker wrapping is the canonical downgrade form for a system item
// that a wire shape or model cannot express as a real system turn: the item
// rides as a user turn wholly enclosed in the markers. Adapters restore the
// system role on parse when a user turn is exactly this form, so the marker
// never surfaces as user text on replay. Substring occurrences never unwrap —
// quoted or embedded content must not be promotable to system priority.
const (
	SystemMarkerOpen  = "<system>\n"
	SystemMarkerClose = "\n</system>"
)

// WrapSystemMarker encloses system-item text in the marker form.
func WrapSystemMarker(text string) string {
	return SystemMarkerOpen + text + SystemMarkerClose
}

// UnwrapSystemMarker returns the inner text when the whole string is exactly
// the marker form.
func UnwrapSystemMarker(text string) (string, bool) {
	if len(text) <= len(SystemMarkerOpen)+len(SystemMarkerClose) ||
		!strings.HasPrefix(text, SystemMarkerOpen) || !strings.HasSuffix(text, SystemMarkerClose) {
		return "", false
	}
	return text[len(SystemMarkerOpen) : len(text)-len(SystemMarkerClose)], true
}
