package usage

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
