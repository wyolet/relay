package scim

import (
	"strings"
)

// maxFilterLen and maxFilterDepth bound the work one filter can cause.
const (
	maxFilterLen   = 4096
	maxFilterDepth = 32
)

// expr is a parsed filter, resolved against one resource schema. It evaluates over a resource's document: lowercase attribute names to a string, a bool, or a []map[string]any for a multi-valued complex attribute.
type expr interface {
	eval(doc map[string]any) bool
}

type andExpr struct{ l, r expr }
type orExpr struct{ l, r expr }
type notExpr struct{ e expr }

func (e andExpr) eval(d map[string]any) bool { return e.l.eval(d) && e.r.eval(d) }
func (e orExpr) eval(d map[string]any) bool  { return e.l.eval(d) || e.r.eval(d) }
func (e notExpr) eval(d map[string]any) bool { return !e.e.eval(d) }

// valuePathExpr is attr[filter]: true when one element of the multi-valued attribute matches the inner filter.
type valuePathExpr struct {
	name  string
	inner expr
}

func (e valuePathExpr) eval(d map[string]any) bool {
	for _, el := range elements(d, e.name) {
		if e.inner.eval(el) {
			return true
		}
	}
	return false
}

func elements(d map[string]any, name string) []map[string]any {
	els, _ := d[name].([]map[string]any)
	return els
}

// parseFilter parses a filter (RFC 7644 section 3.4.2.2) over the attributes schema lets clients filter by. Operators and attribute names are case-insensitive.
func parseFilter(s string, schema *resourceSchema) (expr, error) {
	if len(s) > maxFilterLen {
		return nil, badRequest(typeInvalidFilter, "filter is longer than %d bytes", maxFilterLen)
	}
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	p := &filterParser{toks: toks, schema: schema, attrs: schema.attrs}
	e, err := p.parseOr(0)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tokEOF {
		return nil, badRequest(typeInvalidFilter, "unexpected %q at offset %d", t.text, t.pos)
	}
	return e, nil
}

type filterParser struct {
	toks   []token
	i      int
	schema *resourceSchema
	// attrs is what names resolve against: the resource's attributes, or one attribute's sub-attributes inside a value path.
	attrs map[string]*attrDef
	// inside is the attribute a value path is filtering, "" at the top level.
	inside string
}

func (p *filterParser) peek() token { return p.toks[p.i] }

// next consumes a token; the final EOF is never consumed, so reading past the end keeps returning it.
func (p *filterParser) next() token {
	t := p.toks[p.i]
	if t.kind != tokEOF {
		p.i++
	}
	return t
}

func (p *filterParser) keyword(word string) bool {
	t := p.peek()
	if t.kind == tokWord && strings.EqualFold(t.text, word) {
		p.i++
		return true
	}
	return false
}

func (p *filterParser) parseOr(depth int) (expr, error) {
	if depth > maxFilterDepth {
		return nil, badRequest(typeInvalidFilter, "filter nests deeper than %d", maxFilterDepth)
	}
	l, err := p.parseAnd(depth)
	if err != nil {
		return nil, err
	}
	for p.keyword("or") {
		r, err := p.parseAnd(depth)
		if err != nil {
			return nil, err
		}
		l = orExpr{l, r}
	}
	return l, nil
}

func (p *filterParser) parseAnd(depth int) (expr, error) {
	l, err := p.parseUnary(depth)
	if err != nil {
		return nil, err
	}
	for p.keyword("and") {
		r, err := p.parseUnary(depth)
		if err != nil {
			return nil, err
		}
		l = andExpr{l, r}
	}
	return l, nil
}

func (p *filterParser) parseUnary(depth int) (expr, error) {
	if p.keyword("not") {
		if p.peek().kind != tokLParen {
			return nil, badRequest(typeInvalidFilter, "not must be followed by a parenthesised filter")
		}
		e, err := p.parseUnary(depth + 1)
		if err != nil {
			return nil, err
		}
		return notExpr{e}, nil
	}
	if p.peek().kind == tokLParen {
		p.next()
		e, err := p.parseOr(depth + 1)
		if err != nil {
			return nil, err
		}
		if t := p.next(); t.kind != tokRParen {
			return nil, badRequest(typeInvalidFilter, "missing ) at offset %d", t.pos)
		}
		return e, nil
	}
	return p.parseAttrExpr(depth)
}

func (p *filterParser) parseAttrExpr(depth int) (expr, error) {
	t := p.next()
	if t.kind != tokWord {
		return nil, badRequest(typeInvalidFilter, "expected an attribute at offset %d", t.pos)
	}
	name, sub, ext, err := p.schema.splitPath(t.text)
	if err != nil || ext {
		return nil, badRequest(typeInvalidFilter, "cannot filter by %q", t.text)
	}
	if p.peek().kind == tokLBracket {
		return p.parseValuePath(name, sub, depth)
	}
	ref, err := p.resolve(name, sub)
	if err != nil {
		return nil, err
	}
	opTok := p.next()
	op := strings.ToLower(opTok.text)
	if opTok.kind != tokWord {
		return nil, badRequest(typeInvalidFilter, "expected an operator at offset %d", opTok.pos)
	}
	if op == "pr" {
		return presentExpr{ref}, nil
	}
	if !compareOps[op] {
		return nil, badRequest(typeInvalidFilter, "unknown operator %q", opTok.text)
	}
	value, err := compareValue(p.next())
	if err != nil {
		return nil, err
	}
	return newCompare(ref, op, value)
}

func (p *filterParser) parseValuePath(name, sub string, depth int) (expr, error) {
	def := p.attrs[name]
	if p.inside != "" || sub != "" || def == nil || !def.multi {
		return nil, badRequest(typeInvalidFilter, "%q cannot take a value filter", name)
	}
	p.next()
	outer, outerInside := p.attrs, p.inside
	p.attrs, p.inside = def.subs, name
	inner, err := p.parseOr(depth + 1)
	p.attrs, p.inside = outer, outerInside
	if err != nil {
		return nil, err
	}
	if t := p.next(); t.kind != tokRBracket {
		return nil, badRequest(typeInvalidFilter, "missing ] at offset %d", t.pos)
	}
	return valuePathExpr{name: name, inner: inner}, nil
}

// resolve finds the filterable attribute name.sub names; a multi-valued attribute named alone means its value sub-attribute, as RFC 7644 says.
func (p *filterParser) resolve(name, sub string) (attrRef, error) {
	full := name
	if p.inside != "" {
		full = p.inside + "." + name
	}
	def := p.attrs[name]
	if def != nil && def.multi && sub == "" {
		sub = "value"
	}
	if def != nil && sub != "" {
		def = def.subs[sub]
		full += "." + sub
	}
	if def == nil || !def.filterable {
		return attrRef{}, badRequest(typeInvalidFilter, "cannot filter by %q", full)
	}
	return attrRef{name: name, sub: sub, def: def}, nil
}

// compareValue reads a filter's comparison value: a string or true/false. Numbers, dates and null name no attribute the handler filters by.
func compareValue(t token) (any, error) {
	switch {
	case t.kind == tokString:
		return t.text, nil
	case t.kind == tokWord && strings.EqualFold(t.text, "true"):
		return true, nil
	case t.kind == tokWord && strings.EqualFold(t.text, "false"):
		return false, nil
	default:
		return nil, badRequest(typeInvalidFilter, "unsupported comparison value %q at offset %d", t.text, t.pos)
	}
}
