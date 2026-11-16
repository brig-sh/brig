// Package graphql parses a GraphQL executable document far enough for the
// broker to decide whether a request may go upstream: the operations, their
// selections, and the arguments of each field.
//
// It follows the October 2021 specification and refuses what it does not
// understand, so a document it accepts means to the broker what it means to
// the server. It validates nothing against a schema.
package graphql

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits on a document. A query gh sends nests about ten deep and holds a few
// hundred tokens.
const (
	maxDepth  = 64
	maxTokens = 50_000
)

// Document is a parsed executable document.
type Document struct {
	Operations []*Operation
	Fragments  map[string]*Fragment
}

// Operation is one operation definition. Kind is query, mutation or
// subscription. Name is empty for an anonymous operation.
type Operation struct {
	Kind       string
	Name       string
	Variables  []VariableDefinition
	Selections []Selection
}

// VariableDefinition is $Name: Type = Default. Default is nil when unset.
type VariableDefinition struct {
	Name    string
	Default *Value
}

// Fragment is a named fragment definition.
type Fragment struct {
	Name       string
	On         string
	Selections []Selection
}

// Selection is a field, a fragment spread or an inline fragment. Exactly one
// of Field, Spread and Inline is set.
type Selection struct {
	Field  *Field
	Spread string
	Inline *InlineFragment
}

// Field is a field selection. Alias is empty when unset.
type Field struct {
	Alias      string
	Name       string
	Arguments  []Argument
	Selections []Selection
}

// InlineFragment is "... on Type { }". On is empty when the type is unset.
type InlineFragment struct {
	On         string
	Selections []Selection
}

// Argument is name: value.
type Argument struct {
	Name  string
	Value Value
}

// ValueKind is the kind of a Value.
type ValueKind int

// The kinds of a Value.
const (
	KindVariable ValueKind = iota
	KindInt
	KindFloat
	KindString
	KindBoolean
	KindNull
	KindEnum
	KindList
	KindObject
)

// Value is an input value. Raw holds the variable name, the decoded string,
// or the literal text of a number, boolean or enum. A block string is kept
// raw, with Block set. List holds the items of a list, and Fields the fields
// of an object.
type Value struct {
	Kind   ValueKind
	Raw    string
	Block  bool
	List   []Value
	Fields []Argument
}

// Parse parses src as an executable document. It refuses a type system
// definition, a duplicate operation, fragment, variable, argument or input
// field name, an anonymous operation beside another, a spread of an undefined
// fragment, and a fragment cycle. GitHub refuses each of these too; the
// broker does not rely on it.
func Parse(src string) (*Document, error) {
	p := &parser{lex: lexer{src: src}}
	if err := p.next(); err != nil {
		return nil, err
	}
	doc := &Document{Fragments: map[string]*Fragment{}}
	names := map[string]bool{}
	for p.tok.kind != tokEOF {
		if p.tok.kind == tokName && p.tok.val == "fragment" {
			f, err := p.fragment()
			if err != nil {
				return nil, err
			}
			if doc.Fragments[f.Name] != nil {
				return nil, fmt.Errorf("duplicate fragment %q", f.Name)
			}
			doc.Fragments[f.Name] = f
			continue
		}
		op, err := p.operation()
		if err != nil {
			return nil, err
		}
		if names[op.Name] {
			return nil, fmt.Errorf("duplicate operation %q", op.Name)
		}
		names[op.Name] = true
		doc.Operations = append(doc.Operations, op)
	}
	if len(doc.Operations) == 0 {
		return nil, fmt.Errorf("document has no operation")
	}
	if len(doc.Operations) > 1 && names[""] {
		return nil, fmt.Errorf("anonymous operation must be the only operation in the document")
	}
	if err := doc.checkSpreads(); err != nil {
		return nil, err
	}
	return doc, nil
}

// Operation returns the operation that name selects, as a server does: the
// one of that name, or the only one when name is empty.
func (d *Document) Operation(name string) (*Operation, error) {
	if name == "" {
		if len(d.Operations) != 1 {
			return nil, fmt.Errorf("document has %d operations; set operationName to choose one", len(d.Operations))
		}
		return d.Operations[0], nil
	}
	for _, op := range d.Operations {
		if op.Name == name {
			return op, nil
		}
	}
	return nil, fmt.Errorf("operation %q not found in the document", name)
}

// checkSpreads refuses a spread of an undefined fragment and a fragment that
// reaches itself.
func (d *Document) checkSpreads() error {
	const (
		unseen = iota
		visiting
		done
	)
	state := map[string]int{}
	var visit func(sels []Selection) error
	visit = func(sels []Selection) error {
		for _, s := range sels {
			switch {
			case s.Field != nil:
				if err := visit(s.Field.Selections); err != nil {
					return err
				}
			case s.Inline != nil:
				if err := visit(s.Inline.Selections); err != nil {
					return err
				}
			default:
				f := d.Fragments[s.Spread]
				if f == nil {
					return fmt.Errorf("undefined fragment %q", s.Spread)
				}
				switch state[f.Name] {
				case visiting:
					return fmt.Errorf("fragment %q spreads itself, directly or through another fragment", f.Name)
				case unseen:
					state[f.Name] = visiting
					if err := visit(f.Selections); err != nil {
						return err
					}
					state[f.Name] = done
				}
			}
		}
		return nil
	}
	for _, op := range d.Operations {
		if err := visit(op.Selections); err != nil {
			return err
		}
	}
	for _, f := range d.Fragments {
		if err := visit(f.Selections); err != nil {
			return err
		}
	}
	return nil
}

type parser struct {
	lex   lexer
	tok   token
	depth int
}

func (p *parser) next() error {
	t, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("offset %d: %s", p.tok.pos, fmt.Sprintf(format, args...))
}

// punct consumes the punctuator s.
func (p *parser) punct(s string) error {
	if p.tok.kind != tokPunct || p.tok.val != s {
		return p.errorf("expected %q, found %s", s, p.tok)
	}
	return p.next()
}

// peek reports whether the token is the punctuator s.
func (p *parser) peek(s string) bool {
	return p.tok.kind == tokPunct && p.tok.val == s
}

func (p *parser) name() (string, error) {
	if p.tok.kind != tokName {
		return "", p.errorf("expected a name, found %s", p.tok)
	}
	n := p.tok.val
	return n, p.next()
}

func (p *parser) enter() error {
	p.depth++
	if p.depth > maxDepth {
		return p.errorf("document is nested more than %d levels deep", maxDepth)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) operation() (*Operation, error) {
	op := &Operation{Kind: "query"}
	if p.peek("{") {
		sels, err := p.selectionSet()
		op.Selections = sels
		return op, err
	}
	if p.tok.kind != tokName {
		return nil, p.errorf("expected an operation or fragment, found %s", p.tok)
	}
	switch p.tok.val {
	case "query", "mutation", "subscription":
		op.Kind = p.tok.val
	default:
		return nil, p.errorf("unknown operation type %q; use query or mutation", p.tok.val)
	}
	if err := p.next(); err != nil {
		return nil, err
	}
	if p.tok.kind == tokName {
		op.Name = p.tok.val
		if err := p.next(); err != nil {
			return nil, err
		}
	}
	if p.peek("(") {
		vars, err := p.variableDefinitions()
		if err != nil {
			return nil, err
		}
		op.Variables = vars
	}
	if err := p.directives(); err != nil {
		return nil, err
	}
	sels, err := p.selectionSet()
	op.Selections = sels
	return op, err
}

func (p *parser) variableDefinitions() ([]VariableDefinition, error) {
	if err := p.punct("("); err != nil {
		return nil, err
	}
	var vars []VariableDefinition
	for {
		if err := p.punct("$"); err != nil {
			return nil, err
		}
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		if err := p.punct(":"); err != nil {
			return nil, err
		}
		if err := p.typeRef(); err != nil {
			return nil, err
		}
		v := VariableDefinition{Name: n}
		if p.peek("=") {
			if err := p.next(); err != nil {
				return nil, err
			}
			d, err := p.value(true)
			if err != nil {
				return nil, err
			}
			v.Default = &d
		}
		if err := p.directives(); err != nil {
			return nil, err
		}
		for _, prev := range vars {
			if prev.Name == n {
				return nil, p.errorf("duplicate variable $%s", n)
			}
		}
		vars = append(vars, v)
		if p.peek(")") {
			return vars, p.next()
		}
	}
}

func (p *parser) typeRef() error {
	if err := p.enter(); err != nil {
		return err
	}
	defer p.leave()
	if p.peek("[") {
		if err := p.next(); err != nil {
			return err
		}
		if err := p.typeRef(); err != nil {
			return err
		}
		if err := p.punct("]"); err != nil {
			return err
		}
	} else if _, err := p.name(); err != nil {
		return err
	}
	if p.peek("!") {
		return p.next()
	}
	return nil
}

func (p *parser) fragment() (*Fragment, error) {
	if err := p.next(); err != nil { // fragment
		return nil, err
	}
	n, err := p.name()
	if err != nil {
		return nil, err
	}
	if n == "on" {
		return nil, p.errorf("fragment cannot be named \"on\"")
	}
	if p.tok.kind != tokName || p.tok.val != "on" {
		return nil, p.errorf("expected \"on\", found %s", p.tok)
	}
	if err := p.next(); err != nil {
		return nil, err
	}
	on, err := p.name()
	if err != nil {
		return nil, err
	}
	if err := p.directives(); err != nil {
		return nil, err
	}
	sels, err := p.selectionSet()
	return &Fragment{Name: n, On: on, Selections: sels}, err
}

func (p *parser) selectionSet() ([]Selection, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	if err := p.punct("{"); err != nil {
		return nil, err
	}
	var sels []Selection
	for {
		s, err := p.selection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, s)
		if p.peek("}") {
			return sels, p.next()
		}
	}
}

func (p *parser) selection() (Selection, error) {
	if p.peek("...") {
		if err := p.next(); err != nil {
			return Selection{}, err
		}
		if p.tok.kind == tokName && p.tok.val != "on" {
			n := p.tok.val
			if err := p.next(); err != nil {
				return Selection{}, err
			}
			return Selection{Spread: n}, p.directives()
		}
		in := &InlineFragment{}
		if p.tok.kind == tokName { // on
			if err := p.next(); err != nil {
				return Selection{}, err
			}
			on, err := p.name()
			if err != nil {
				return Selection{}, err
			}
			in.On = on
		}
		if err := p.directives(); err != nil {
			return Selection{}, err
		}
		sels, err := p.selectionSet()
		in.Selections = sels
		return Selection{Inline: in}, err
	}
	f := &Field{}
	n, err := p.name()
	if err != nil {
		return Selection{}, err
	}
	f.Name = n
	if p.peek(":") {
		if err := p.next(); err != nil {
			return Selection{}, err
		}
		if f.Name, err = p.name(); err != nil {
			return Selection{}, err
		}
		f.Alias = n
	}
	if p.peek("(") {
		if f.Arguments, err = p.arguments(false); err != nil {
			return Selection{}, err
		}
	}
	if err := p.directives(); err != nil {
		return Selection{}, err
	}
	if p.peek("{") {
		if f.Selections, err = p.selectionSet(); err != nil {
			return Selection{}, err
		}
	}
	return Selection{Field: f}, nil
}

func (p *parser) arguments(constant bool) ([]Argument, error) {
	if err := p.punct("("); err != nil {
		return nil, err
	}
	var args []Argument
	for {
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		if err := p.punct(":"); err != nil {
			return nil, err
		}
		v, err := p.value(constant)
		if err != nil {
			return nil, err
		}
		for _, prev := range args {
			if prev.Name == n {
				return nil, p.errorf("duplicate argument %q", n)
			}
		}
		args = append(args, Argument{Name: n, Value: v})
		if p.peek(")") {
			return args, p.next()
		}
	}
}

func (p *parser) directives() error {
	for p.peek("@") {
		if err := p.next(); err != nil {
			return err
		}
		if _, err := p.name(); err != nil {
			return err
		}
		if p.peek("(") {
			if _, err := p.arguments(false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) value(constant bool) (Value, error) {
	if err := p.enter(); err != nil {
		return Value{}, err
	}
	defer p.leave()
	t := p.tok
	switch {
	case t.kind == tokPunct && t.val == "$":
		if constant {
			return Value{}, p.errorf("variable not allowed in a default value")
		}
		if err := p.next(); err != nil {
			return Value{}, err
		}
		n, err := p.name()
		return Value{Kind: KindVariable, Raw: n}, err
	case t.kind == tokInt:
		return Value{Kind: KindInt, Raw: t.val}, p.next()
	case t.kind == tokFloat:
		return Value{Kind: KindFloat, Raw: t.val}, p.next()
	case t.kind == tokString:
		return Value{Kind: KindString, Raw: t.val}, p.next()
	case t.kind == tokBlockString:
		return Value{Kind: KindString, Raw: t.val, Block: true}, p.next()
	case t.kind == tokName:
		k := KindEnum
		switch t.val {
		case "true", "false":
			k = KindBoolean
		case "null":
			k = KindNull
		}
		return Value{Kind: k, Raw: t.val}, p.next()
	case t.kind == tokPunct && t.val == "[":
		if err := p.next(); err != nil {
			return Value{}, err
		}
		v := Value{Kind: KindList}
		for !p.peek("]") {
			item, err := p.value(constant)
			if err != nil {
				return Value{}, err
			}
			v.List = append(v.List, item)
		}
		return v, p.next()
	case t.kind == tokPunct && t.val == "{":
		if err := p.next(); err != nil {
			return Value{}, err
		}
		v := Value{Kind: KindObject}
		for !p.peek("}") {
			n, err := p.name()
			if err != nil {
				return Value{}, err
			}
			if err := p.punct(":"); err != nil {
				return Value{}, err
			}
			fv, err := p.value(constant)
			if err != nil {
				return Value{}, err
			}
			for _, prev := range v.Fields {
				if prev.Name == n {
					return Value{}, p.errorf("duplicate input field %q", n)
				}
			}
			v.Fields = append(v.Fields, Argument{Name: n, Value: fv})
		}
		return v, p.next()
	}
	return Value{}, p.errorf("expected a value, found %s", t)
}

type tokKind int

const (
	tokEOF tokKind = iota
	tokPunct
	tokName
	tokInt
	tokFloat
	tokString
	tokBlockString
)

type token struct {
	kind tokKind
	val  string
	pos  int
}

func (t token) String() string {
	if t.kind == tokEOF {
		return "end of document"
	}
	return strconv.Quote(t.val)
}

type lexer struct {
	src    string
	pos    int
	tokens int
}

func (l *lexer) next() (token, error) {
	if err := l.skipIgnored(); err != nil {
		return token{}, err
	}
	if l.pos >= len(l.src) {
		return token{kind: tokEOF, pos: l.pos}, nil
	}
	l.tokens++
	if l.tokens > maxTokens {
		return token{}, fmt.Errorf("document has more than %d tokens", maxTokens)
	}
	start := l.pos
	c := l.src[l.pos]
	switch {
	case strings.IndexByte("!$&():=@[]{}|", c) >= 0:
		l.pos++
		return token{tokPunct, string(c), start}, nil
	case c == '.':
		if strings.HasPrefix(l.src[l.pos:], "...") {
			l.pos += 3
			return token{tokPunct, "...", start}, nil
		}
		return token{}, fmt.Errorf("offset %d: unexpected \".\"; a fragment spread is \"...\"", start)
	case nameStart(c):
		for l.pos < len(l.src) && nameContinue(l.src[l.pos]) {
			l.pos++
		}
		return token{tokName, l.src[start:l.pos], start}, nil
	case c == '-' || isDigit(c):
		return l.number()
	case c == '"':
		if strings.HasPrefix(l.src[l.pos:], `"""`) {
			return l.blockString()
		}
		return l.string()
	}
	return token{}, fmt.Errorf("offset %d: unexpected character %q", start, c)
}

// skipIgnored skips the byte order mark, white space, line terminators,
// commas and comments.
func (l *lexer) skipIgnored() error {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			l.pos++
		case strings.HasPrefix(l.src[l.pos:], "\uFEFF"):
			l.pos += len("\uFEFF")
		case c == '#':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' && l.src[l.pos] != '\r' {
				l.pos++
			}
		default:
			return nil
		}
	}
	return nil
}

func (l *lexer) number() (token, error) {
	start := l.pos
	kind := tokInt
	if l.src[l.pos] == '-' {
		l.pos++
	}
	if l.pos >= len(l.src) || !isDigit(l.src[l.pos]) {
		return token{}, fmt.Errorf("offset %d: number has no digits", start)
	}
	if l.src[l.pos] == '0' {
		l.pos++
		if l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			return token{}, fmt.Errorf("offset %d: number has a leading zero", start)
		}
	} else {
		l.digits()
	}
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		kind = tokFloat
		l.pos++
		if l.digits() == 0 {
			return token{}, fmt.Errorf("offset %d: fraction has no digits", start)
		}
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		kind = tokFloat
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		if l.digits() == 0 {
			return token{}, fmt.Errorf("offset %d: exponent has no digits", start)
		}
	}
	// A name or dot cannot follow a number: 1a and 1.2.3 are errors.
	if l.pos < len(l.src) && (nameStart(l.src[l.pos]) || l.src[l.pos] == '.') {
		return token{}, fmt.Errorf("offset %d: number is followed by %q; separate them with a space", start, l.src[l.pos])
	}
	return token{kind, l.src[start:l.pos], start}, nil
}

func (l *lexer) digits() int {
	n := 0
	for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
		l.pos++
		n++
	}
	return n
}

// string reads a quoted string and decodes its escapes. It refuses the
// braced \u{...} escape, which servers read differently.
func (l *lexer) string() (token, error) {
	start := l.pos
	l.pos++
	var b strings.Builder
	for {
		if l.pos >= len(l.src) {
			return token{}, fmt.Errorf("offset %d: unterminated string", start)
		}
		c := l.src[l.pos]
		switch {
		case c == '"':
			l.pos++
			return token{tokString, b.String(), start}, nil
		case c == '\n' || c == '\r':
			return token{}, fmt.Errorf("offset %d: string contains a line break; use \\n or a block string", start)
		case c < 0x20 && c != '\t':
			return token{}, fmt.Errorf("offset %d: string contains a control character; escape it", start)
		case c == '\\':
			if l.pos+1 >= len(l.src) {
				return token{}, fmt.Errorf("offset %d: unterminated string", start)
			}
			e := l.src[l.pos+1]
			l.pos += 2
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if l.pos+4 > len(l.src) {
					return token{}, fmt.Errorf("offset %d: \\u escape needs four hex digits", start)
				}
				n, err := strconv.ParseUint(l.src[l.pos:l.pos+4], 16, 16)
				if err != nil {
					return token{}, fmt.Errorf("offset %d: \\u escape needs four hex digits", start)
				}
				r := rune(n)
				if r >= 0xD800 && r <= 0xDFFF {
					return token{}, fmt.Errorf("offset %d: \\u escape is a surrogate; send the character as UTF-8", start)
				}
				b.WriteRune(r)
				l.pos += 4
			default:
				return token{}, fmt.Errorf("offset %d: invalid escape \\%c", start, e)
			}
		default:
			r, size := utf8.DecodeRuneInString(l.src[l.pos:])
			if r == utf8.RuneError && size == 1 {
				return token{}, fmt.Errorf("offset %d: string is not valid UTF-8", start)
			}
			b.WriteString(l.src[l.pos : l.pos+size])
			l.pos += size
		}
	}
}

// blockString reads a """ string. Its value matters to the broker only as a
// literal argument, so it is returned raw, without the indentation removed.
func (l *lexer) blockString() (token, error) {
	start := l.pos
	l.pos += 3
	var b strings.Builder
	for {
		if l.pos >= len(l.src) {
			return token{}, fmt.Errorf("offset %d: unterminated block string", start)
		}
		switch {
		case strings.HasPrefix(l.src[l.pos:], `\"""`):
			b.WriteString(`"""`)
			l.pos += 4
		case strings.HasPrefix(l.src[l.pos:], `"""`):
			l.pos += 3
			return token{tokBlockString, b.String(), start}, nil
		default:
			b.WriteByte(l.src[l.pos])
			l.pos++
		}
	}
}

func nameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func nameContinue(c byte) bool { return nameStart(c) || isDigit(c) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
