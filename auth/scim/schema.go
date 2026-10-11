package scim

import (
	"strings"
)

// The URNs of RFC 7643 and RFC 7644 the handler speaks.
const (
	schemaUser         = "urn:ietf:params:scim:schemas:core:2.0:User"
	schemaGroup        = "urn:ietf:params:scim:schemas:core:2.0:Group"
	schemaListResponse = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	schemaPatchOp      = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	schemaError        = "urn:ietf:params:scim:api:messages:2.0:Error"
	schemaSPConfig     = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	schemaResourceType = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	schemaSchema       = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	contentType        = "application/scim+json"
)

type attrType int

const (
	typeString attrType = iota
	typeBool
	typeComplex
)

// attrDef describes one attribute the handler filters or patches by.
type attrDef struct {
	typ       attrType
	caseExact bool
	// multi marks a multi-valued complex attribute; subs are its sub-attributes.
	multi      bool
	subs       map[string]*attrDef
	filterable bool
}

// resourceSchema is the part of a core schema the handler implements, keyed by lowercase attribute name.
type resourceSchema struct {
	urn   string
	attrs map[string]*attrDef
	// ignored are core attributes the store cannot hold yet; a write naming one is accepted and has no effect.
	ignored map[string]bool
}

var userSchema = &resourceSchema{
	urn: schemaUser,
	attrs: map[string]*attrDef{
		"id":         {typ: typeString, caseExact: true, filterable: true},
		"username":   {typ: typeString, filterable: true},
		"externalid": {typ: typeString, caseExact: true, filterable: true},
		"active":     {typ: typeBool},
		"emails": {typ: typeComplex, multi: true, subs: map[string]*attrDef{
			"value":   {typ: typeString, filterable: true},
			"type":    {typ: typeString, filterable: true},
			"primary": {typ: typeBool, filterable: true},
		}},
		"groups": {typ: typeComplex, multi: true, subs: map[string]*attrDef{"value": {typ: typeString, caseExact: true}}},
	},
	ignored: setOf("name", "displayname", "nickname", "profileurl", "title", "usertype", "preferredlanguage",
		"locale", "timezone", "password", "phonenumbers", "ims", "photos", "addresses", "entitlements", "roles",
		"x509certificates"),
}

var groupSchema = &resourceSchema{
	urn: schemaGroup,
	attrs: map[string]*attrDef{
		"id":          {typ: typeString, caseExact: true, filterable: true},
		"displayname": {typ: typeString, filterable: true},
		"externalid":  {typ: typeString, caseExact: true, filterable: true},
		"members": {typ: typeComplex, multi: true, subs: map[string]*attrDef{
			"value": {typ: typeString, caseExact: true, filterable: true},
		}},
	},
}

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// splitPath splits an attribute path into its lowercase name and sub-attribute, dropping the resource's own schema URN. ext reports an attribute of another schema, such as the enterprise extension, which the handler does not hold.
func (s *resourceSchema) splitPath(path string) (name, sub string, ext bool, err error) {
	if i := strings.LastIndexByte(path, ':'); i >= 0 {
		if !strings.EqualFold(path[:i], s.urn) {
			return "", "", true, nil
		}
		path = path[i+1:]
	}
	name, sub, _ = strings.Cut(strings.ToLower(path), ".")
	if !validAttrName(name) || (sub != "" && !validAttrName(sub)) {
		return "", "", false, badRequest(typeInvalidPath, "%q is not an attribute path", path)
	}
	return name, sub, false, nil
}

// validAttrName is RFC 7643's ATTRNAME, plus "$ref".
func validAttrName(s string) bool {
	if s == "$ref" {
		return true
	}
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
