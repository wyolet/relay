package scim

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// The page size a list returns when the client asks for none, and the most it returns at once.
const (
	defaultCount = 100
	maxCount     = 200
)

// listQuery is a list request's filter, page and attribute selection.
type listQuery struct {
	filter     expr
	startIndex int
	count      int
	sel        selection
}

func parseListQuery(q url.Values, schema *resourceSchema) (listQuery, error) {
	lq := listQuery{startIndex: 1, count: defaultCount}
	if f := strings.TrimSpace(q.Get("filter")); f != "" {
		e, err := parseFilter(f, schema)
		if err != nil {
			return lq, err
		}
		lq.filter = e
	}
	var err error
	if lq.startIndex, err = intParam(q, "startIndex", 1); err != nil {
		return lq, err
	}
	// RFC 7644 section 3.4.2.4: a startIndex below 1 is read as 1 and a negative count as 0.
	lq.startIndex = max(lq.startIndex, 1)
	if lq.count, err = intParam(q, "count", defaultCount); err != nil {
		return lq, err
	}
	lq.count = min(max(lq.count, 0), maxCount)
	lq.sel, err = parseSelection(q, schema)
	return lq, err
}

func intParam(q url.Values, name string, def int) (int, error) {
	s := strings.TrimSpace(q.Get(name))
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, badRequest(typeInvalidValue, "%s must be an integer", name)
	}
	return n, nil
}

type listResponse struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    []any    `json:"Resources"`
}

// page cuts the matching resources down to the requested page.
func page(matching []any, lq listQuery) listResponse {
	out := listResponse{Schemas: []string{schemaListResponse}, TotalResults: len(matching), StartIndex: lq.startIndex, Resources: []any{}}
	from := lq.startIndex - 1
	if from < len(matching) && lq.count > 0 {
		to := min(from+lq.count, len(matching))
		out.Resources = matching[from:to]
	}
	out.ItemsPerPage = len(out.Resources)
	return out
}

// selection is the attributes or excludedAttributes parameter, as lowercase top-level attribute names. A sub-attribute selects its whole parent.
type selection struct {
	only, except map[string]bool
}

func parseSelection(q url.Values, schema *resourceSchema) (selection, error) {
	var s selection
	var err error
	if s.only, err = attrList(q.Get("attributes"), schema); err != nil {
		return s, err
	}
	s.except, err = attrList(q.Get("excludedAttributes"), schema)
	return s, err
}

func attrList(param string, schema *resourceSchema) (map[string]bool, error) {
	if strings.TrimSpace(param) == "" {
		return nil, nil
	}
	out := map[string]bool{}
	for _, p := range strings.Split(param, ",") {
		name, _, ext, err := schema.splitPath(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		if !ext {
			out[name] = true
		}
	}
	return out, nil
}

// apply projects a rendered resource. id and schemas are always returned, as RFC 7643 requires.
func (s selection) apply(resource any) (any, error) {
	if s.only == nil && s.except == nil {
		return resource, nil
	}
	raw, err := json.Marshal(resource)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k := range m {
		lk := strings.ToLower(k)
		if lk == "id" || lk == "schemas" {
			continue
		}
		if (s.only != nil && !s.only[lk]) || s.except[lk] {
			delete(m, k)
		}
	}
	return m, nil
}
