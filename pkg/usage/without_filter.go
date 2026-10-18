package usage

// dimFilters maps every group dimension that EventQuery can filter on to the code clearing that filter. credential_id, project, team, principal and extras.<key> have no filter, so they are absent; tags.<key> is handled in WithoutFilter.
var dimFilters = map[string]func(*EventQuery){
	"relay_key_hash": func(q *EventQuery) { q.RelayKeyHash = nil },
	"policy_id":      func(q *EventQuery) { q.PolicyID = nil },
	"model_id":       func(q *EventQuery) { q.ModelID = nil },
	"host_id":        func(q *EventQuery) { q.HostID = nil },
	"host_key_id":    func(q *EventQuery) { q.HostKeyID = nil },
	"source":         func(q *EventQuery) { q.Source = nil },
	"finish_reason":  func(q *EventQuery) { q.FinishReason = nil },
	"error_kind":     func(q *EventQuery) { q.ErrorKind = nil },
	"model":          func(q *EventQuery) { q.Model = nil },
	"host":           func(q *EventQuery) { q.Host = nil },
	"policy":         func(q *EventQuery) { q.Policy = nil },
	"provider":       func(q *EventQuery) { q.Provider = nil },
	"project_id":     func(q *EventQuery) { q.ProjectID = nil },
	"team_id":        func(q *EventQuery) { q.TeamID = nil },
	"principal_id":   func(q *EventQuery) { q.PrincipalID = nil },
	"relay_key":      func(q *EventQuery) { q.RelayKey = nil },
}

// WithoutFilter returns q with its filter on dim removed and every other filter kept. Scope fields are never cleared, and neither are filters that only touch dim indirectly (q, status, error).
func WithoutFilter(q EventQuery, dim string) EventQuery {
	if drop, ok := dimFilters[dim]; ok {
		drop(&q)
		return q
	}
	if column, key, ok := MapGroupKey(dim); ok && column == "tags" {
		if _, has := q.Tags[key]; has {
			tags := make(map[string][]string, len(q.Tags)-1)
			for k, v := range q.Tags {
				if k != key {
					tags[k] = v
				}
			}
			q.Tags = tags
		}
	}
	return q
}
