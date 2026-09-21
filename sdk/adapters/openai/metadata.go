package openai

// Charset rules for request metadata, shared by the CC and Responses parsers.
// The key rule is identical for both; the value rule is not — the CC path also
// bans ',' and '=' because that metadata round-trips through the comma/equals
// separated X-WR-Metadata header form.

func validMetaKey(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func validMetaValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7E || c == ',' || c == '=' {
			return false
		}
	}
	return true
}

func responsesValidMetaValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}
