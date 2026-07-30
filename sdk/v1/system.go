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

// SplitHoistedSystem removes hoist-flagged system/developer messages from
// items and returns their texts joined with "\n". Serializers merge the text
// into the shape's start-of-conversation system form (top-level system,
// systemInstruction, leading system message). The original slice is returned
// untouched when nothing is flagged.
func SplitHoistedSystem(items []Item) ([]Item, string) {
	flagged := false
	for _, it := range items {
		if m, ok := it.(*Message); ok && m.Hoist && (m.Role == RoleSystem || m.Role == RoleDeveloper) {
			flagged = true
			break
		}
	}
	if !flagged {
		return items, ""
	}
	var texts []string
	rest := make([]Item, 0, len(items))
	for _, it := range items {
		if m, ok := it.(*Message); ok && m.Hoist && (m.Role == RoleSystem || m.Role == RoleDeveloper) {
			var sb strings.Builder
			for _, p := range m.Content {
				switch tp := p.(type) {
				case *TextPart:
					sb.WriteString(tp.Text)
				case *OutputTextPart:
					sb.WriteString(tp.Text)
				}
			}
			if s := sb.String(); s != "" {
				texts = append(texts, s)
			}
			continue
		}
		rest = append(rest, it)
	}
	return rest, strings.Join(texts, "\n")
}
