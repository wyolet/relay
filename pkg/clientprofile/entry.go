package clientprofile

import (
	"strconv"
	"strings"
)

// Rendering helpers shared by more than one profile's list projection. Nothing here is client-specific: it turns a ModelEntry into the label and subtitle text every picker wants, in whatever document the profile serialises.

// entryLabel names the row in a picker, which shows a display name and nothing else to tell rows apart: every snapshot of one model carries the parent's display name, so the non-pointer ones would render as identical rows without the snapshot suffix appended here.
func entryLabel(e ModelEntry) string {
	if e.Pointer {
		return e.DisplayName
	}
	suffix := e.ID
	switch {
	case e.ID == e.Model:
		suffix = "base"
	case e.Model != "":
		suffix = strings.TrimPrefix(e.ID, e.Model+"-")
	}
	return e.DisplayName + " (" + suffix + ")"
}

// hostsDescription renders the picker subtitle: every host serving the entry, with its rates when the binding is priced.
func hostsDescription(hosts []ModelHost) string {
	parts := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if !h.Priced {
			parts = append(parts, h.Name)
			continue
		}
		parts = append(parts, h.Name+" · $"+usdRate(h.InputUSDPerMtok)+"/$"+usdRate(h.OutputUSDPerMtok)+" per Mtok")
	}
	return strings.Join(parts, ", ")
}

// usdRate formats a per-Mtok amount with at most two decimals and no trailing zeros.
func usdRate(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}
