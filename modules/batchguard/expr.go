//go:build !no_batchguard

package batchguard

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cast"
	"github.com/tokibase/tokibase/core"
)

// Limits of the expression language.
const (
	MaxExprLen  = 2048
	maxDepth    = 32
	EvalTimeout = 100 * time.Millisecond
)

// The language has no loops, no assignment and no user defined functions.
// Grammar (see docs/modules/batchguard.md):
//
//	expr    = or
//	or      = and { "||" and }
//	and     = cmp { "&&" cmp }
//	cmp     = add [ ("==" | "!=" | "<" | "<=" | ">" | ">=") add ]
//	add     = mul { ("+" | "-") mul }
//	mul     = unary { ("*" | "/") unary }
//	unary   = ("!" | "-") unary | primary
//	primary = number | string | true | false | null | bareword | "(" expr ")"
//	        | "@request.auth." name | func "(" [ expr { "," expr } ] ")" [ "." name { "." name } ]
type node interface{}

type (
	litNode   struct{ v any }
	unaryNode struct {
		op byte
		x  node
	}
	binNode struct {
		op   string
		l, r node
	}
	callNode struct {
		name string
		args []node
		path []string // only for req(i)
	}
	authNode struct{ field string }
)

var funcs = map[string]struct{ min, max int }{
	"count":  {1, 2},
	"sum":    {2, 3},
	"all":    {4, 4},
	"exists": {3, 3},
	"stock":  {5, 5},
	"req":    {1, 1},
}

type tok struct {
	kind byte // n number, s string, i ident, o operator, e end
	text string
	num  float64
}

func lex(src string) ([]tok, error) {
	var out []tok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
				j++
			}
			f, err := strconv.ParseFloat(src[i:j], 64)
			if err != nil {
				return nil, fmt.Errorf("bad number %q", src[i:j])
			}
			out = append(out, tok{kind: 'n', num: f, text: src[i:j]})
			i = j
		case c == '"' || c == '\'':
			j := i + 1
			var sb strings.Builder
			for ; j < len(src) && src[j] != c; j++ {
				if src[j] == '\\' && j+1 < len(src) {
					j++
				}
				sb.WriteByte(src[j])
			}
			if j >= len(src) {
				return nil, errors.New("unterminated string")
			}
			out = append(out, tok{kind: 's', text: sb.String()})
			i = j + 1
		case isIdent(c) || c == '@':
			j := i + 1
			for j < len(src) && (isIdent(src[j]) || src[j] >= '0' && src[j] <= '9' || (c == '@' && src[j] == '.')) {
				j++
			}
			out = append(out, tok{kind: 'i', text: src[i:j]})
			i = j
		default:
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			switch two {
			case "==", "!=", "<=", ">=", "&&", "||":
				out = append(out, tok{kind: 'o', text: two})
				i += 2
				continue
			}
			if strings.IndexByte("<>+-*/!(),.", c) < 0 {
				return nil, fmt.Errorf("unexpected character %q", string(c))
			}
			out = append(out, tok{kind: 'o', text: string(c)})
			i++
		}
	}
	return append(out, tok{kind: 'e'}), nil
}

func isIdent(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

type parser struct {
	t     []tok
	p     int
	depth int
}

// Parse compiles src. It is what a save of `_batch_rules` runs.
func Parse(src string) (node, error) {
	if strings.TrimSpace(src) == "" {
		return nil, errors.New("empty expression")
	}
	if len(src) > MaxExprLen {
		return nil, fmt.Errorf("expression is longer than %d bytes", MaxExprLen)
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	ps := &parser{t: toks}
	n, err := ps.or()
	if err != nil {
		return nil, err
	}
	if ps.cur().kind != 'e' {
		return nil, fmt.Errorf("unexpected %q", ps.cur().text)
	}
	return n, nil
}

func (p *parser) cur() tok { return p.t[p.p] }
func (p *parser) isOp(s ...string) (string, bool) {
	c := p.cur()
	if c.kind != 'o' {
		return "", false
	}
	for _, x := range s {
		if c.text == x {
			return x, true
		}
	}
	return "", false
}

func (p *parser) or() (node, error) {
	l, err := p.and()
	for err == nil {
		if _, ok := p.isOp("||"); !ok {
			break
		}
		p.p++
		var r node
		if r, err = p.and(); err == nil {
			l = binNode{"||", l, r}
		}
	}
	return l, err
}

func (p *parser) and() (node, error) {
	l, err := p.cmp()
	for err == nil {
		if _, ok := p.isOp("&&"); !ok {
			break
		}
		p.p++
		var r node
		if r, err = p.cmp(); err == nil {
			l = binNode{"&&", l, r}
		}
	}
	return l, err
}

func (p *parser) cmp() (node, error) {
	l, err := p.add()
	if err != nil {
		return nil, err
	}
	if op, ok := p.isOp("==", "!=", "<", "<=", ">", ">="); ok {
		p.p++
		r, err := p.add()
		if err != nil {
			return nil, err
		}
		return binNode{op, l, r}, nil
	}
	return l, nil
}

func (p *parser) add() (node, error) {
	l, err := p.mul()
	for err == nil {
		op, ok := p.isOp("+", "-")
		if !ok {
			break
		}
		p.p++
		var r node
		if r, err = p.mul(); err == nil {
			l = binNode{op, l, r}
		}
	}
	return l, err
}

func (p *parser) mul() (node, error) {
	l, err := p.unary()
	for err == nil {
		op, ok := p.isOp("*", "/")
		if !ok {
			break
		}
		p.p++
		var r node
		if r, err = p.unary(); err == nil {
			l = binNode{op, l, r}
		}
	}
	return l, err
}

func (p *parser) unary() (node, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxDepth {
		return nil, errors.New("expression is nested too deeply")
	}
	if op, ok := p.isOp("!", "-"); ok {
		p.p++
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return unaryNode{op[0], x}, nil
	}
	return p.primary()
}

func (p *parser) primary() (node, error) {
	c := p.cur()
	switch c.kind {
	case 'n':
		p.p++
		return litNode{c.num}, nil
	case 's':
		p.p++
		return litNode{c.text}, nil
	case 'i':
		p.p++
		switch {
		case c.text == "true":
			return litNode{true}, nil
		case c.text == "false":
			return litNode{false}, nil
		case c.text == "null":
			return litNode{nil}, nil
		case strings.HasPrefix(c.text, "@"):
			f, ok := strings.CutPrefix(c.text, "@request.auth.")
			if !ok || f == "" {
				return nil, fmt.Errorf("unknown variable %q (only @request.auth.<field>)", c.text)
			}
			return authNode{f}, nil
		}
		if _, ok := p.isOp("("); ok {
			return p.call(c.text)
		}
		return litNode{c.text}, nil // bareword = string (collection and field names)
	case 'o':
		if c.text == "(" {
			p.p++
			p.depth++
			defer func() { p.depth-- }()
			if p.depth > maxDepth {
				return nil, errors.New("expression is nested too deeply")
			}
			n, err := p.or()
			if err != nil {
				return nil, err
			}
			if _, ok := p.isOp(")"); !ok {
				return nil, errors.New("missing )")
			}
			p.p++
			return n, nil
		}
	}
	if c.kind == 'e' {
		return nil, errors.New("unexpected end of expression")
	}
	return nil, fmt.Errorf("unexpected %q", c.text)
}

func (p *parser) call(name string) (node, error) {
	spec, ok := funcs[name]
	if !ok {
		return nil, fmt.Errorf("unknown function %q", name)
	}
	p.p++ // (
	var args []node
	if _, ok := p.isOp(")"); !ok {
		for {
			a, err := p.or()
			if err != nil {
				return nil, err
			}
			args = append(args, a)
			if _, ok := p.isOp(","); ok {
				p.p++
				continue
			}
			break
		}
	}
	if _, ok := p.isOp(")"); !ok {
		return nil, fmt.Errorf("missing ) after %s(", name)
	}
	p.p++
	if len(args) < spec.min || len(args) > spec.max {
		return nil, fmt.Errorf("%s() takes %d..%d arguments, got %d", name, spec.min, spec.max, len(args))
	}
	cn := callNode{name: name, args: args}
	if name == "req" {
		for {
			if _, ok := p.isOp("."); !ok {
				break
			}
			p.p++
			c := p.cur()
			if c.kind != 'i' {
				return nil, errors.New("expected a name after .")
			}
			p.p++
			cn.path = append(cn.path, c.text)
		}
		if len(cn.path) == 0 {
			return nil, errors.New("req(i) needs a path: req(i).body.<field>, .method, .collection or .id")
		}
		switch cn.path[0] {
		case "body":
		case "method", "collection", "id":
			if len(cn.path) != 1 {
				return nil, fmt.Errorf("req(i).%s has no members", cn.path[0])
			}
		default:
			return nil, fmt.Errorf("req(i).%s: use body, method, collection or id", cn.path[0])
		}
	}
	return cn, nil
}

// referencedNames adds every string literal and `req(i).body.<path>` segment
// of the expression to set: the names a rule may use as field names.
func referencedNames(n node, set map[string]bool) {
	switch x := n.(type) {
	case litNode:
		if str, ok := x.v.(string); ok {
			set[str] = true
		}
	case unaryNode:
		referencedNames(x.x, set)
	case binNode:
		referencedNames(x.l, set)
		referencedNames(x.r, set)
	case callNode:
		for _, a := range x.args {
			referencedNames(a, set)
		}
		for _, p := range x.path {
			set[p] = true
		}
	}
}

// ----- evaluation -----

type reqView struct {
	Index      int
	Collection string
	Method     string
	ID         string
	Data       map[string]any
	Deleted    bool
	Upsert     bool // a PUT upsert; Method holds the resolved POST or PATCH
}

type evalCtx struct {
	app      core.App
	reqs     []reqView
	auth     *core.Record
	deadline time.Time
}

var errTimeout = errors.New("evaluation timed out")

// EvalBool parses and evaluates src (used by tests and the CLI).
func evalBool(n node, c *evalCtx) (bool, error) {
	v, err := c.eval(n)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, errors.New("an assertion must evaluate to true or false")
	}
	return b, nil
}

func (c *evalCtx) eval(n node) (any, error) {
	if !c.deadline.IsZero() && time.Now().After(c.deadline) {
		return nil, errTimeout
	}
	switch x := n.(type) {
	case litNode:
		return x.v, nil
	case authNode:
		if c.auth == nil {
			return "", nil
		}
		switch x.field {
		case "id":
			return c.auth.Id, nil
		case "collectionName", "collection":
			return c.auth.Collection().Name, nil
		}
		return c.auth.Get(x.field), nil
	case unaryNode:
		v, err := c.eval(x.x)
		if err != nil {
			return nil, err
		}
		if x.op == '!' {
			b, ok := v.(bool)
			if !ok {
				return nil, errors.New("! needs a boolean")
			}
			return !b, nil
		}
		f, ok := toNum(v)
		if !ok {
			return nil, errors.New("unary - needs a number")
		}
		return -f, nil
	case binNode:
		return c.evalBin(x)
	case callNode:
		return c.evalCall(x)
	}
	return nil, errors.New("bad expression")
}

func (c *evalCtx) evalBin(x binNode) (any, error) {
	l, err := c.eval(x.l)
	if err != nil {
		return nil, err
	}
	if x.op == "&&" || x.op == "||" {
		lb, ok := l.(bool)
		if !ok {
			return nil, fmt.Errorf("%s needs booleans", x.op)
		}
		if x.op == "&&" && !lb {
			return false, nil
		}
		if x.op == "||" && lb {
			return true, nil
		}
		r, err := c.eval(x.r)
		if err != nil {
			return nil, err
		}
		rb, ok := r.(bool)
		if !ok {
			return nil, fmt.Errorf("%s needs booleans", x.op)
		}
		return rb, nil
	}
	r, err := c.eval(x.r)
	if err != nil {
		return nil, err
	}
	switch x.op {
	case "+", "-", "*", "/":
		a, ok1 := toNum(l)
		b, ok2 := toNum(r)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("%s needs numbers", x.op)
		}
		var res float64
		switch x.op {
		case "+":
			res = a + b
		case "-":
			res = a - b
		case "*":
			res = a * b
		default:
			if b == 0 {
				return nil, errors.New("division by zero")
			}
			res = a / b
		}
		if math.IsNaN(res) || math.IsInf(res, 0) {
			return nil, errors.New("arithmetic overflow")
		}
		return res, nil
	}
	return compare(l, x.op, r, false)
}

func toNum(v any) (float64, bool) {
	switch t := v.(type) {
	case nil, bool:
		return 0, false
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	f, err := cast.ToFloat64E(v)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func bothNumericStrings(l, r any) bool {
	ls, ok1 := l.(string)
	rs, ok2 := r.(string)
	if !ok1 || !ok2 {
		return false
	}
	_, a := toNum(ls)
	_, b := toNum(rs)
	return a && b
}

func isNumber(v any) bool {
	switch v.(type) {
	case string, nil, bool:
		return false
	}
	_, ok := toNum(v)
	return ok
}

// compare: nil only supports == and != (an ordering with nil is false, so a
// missing field never satisfies a bound). When one side is a number the other
// is parsed as a number. Two strings compare numerically only when
// numericStrings is set (the field is number typed) and both parse as
// numbers; every other string pair compares lexically.
func compare(l any, op string, r any, numericStrings bool) (any, error) {
	if l == nil || r == nil {
		switch op {
		case "==":
			return l == nil && r == nil, nil
		case "!=":
			return !(l == nil && r == nil), nil
		}
		return false, nil
	}
	var cmp int
	lb, lIsB := l.(bool)
	rb, rIsB := r.(bool)
	switch {
	case lIsB || rIsB:
		if !(lIsB && rIsB) || (op != "==" && op != "!=") {
			if op == "==" {
				return false, nil
			}
			if op == "!=" {
				return true, nil
			}
			return nil, errors.New("cannot order booleans")
		}
		if lb == rb {
			cmp = 0
		} else {
			cmp = 1
		}
	case isNumber(l) || isNumber(r) || (numericStrings && bothNumericStrings(l, r)):
		a, ok1 := toNum(l)
		b, ok2 := toNum(r)
		if !ok1 || !ok2 {
			if op == "==" {
				return false, nil
			}
			if op == "!=" {
				return true, nil
			}
			return nil, fmt.Errorf("cannot compare %v %s %v", l, op, r)
		}
		switch {
		case a < b:
			cmp = -1
		case a > b:
			cmp = 1
		}
	default:
		cmp = strings.Compare(cast.ToString(l), cast.ToString(r))
	}
	switch op {
	case "==":
		return cmp == 0, nil
	case "!=":
		return cmp != 0, nil
	case "<":
		return cmp < 0, nil
	case "<=":
		return cmp <= 0, nil
	case ">":
		return cmp > 0, nil
	case ">=":
		return cmp >= 0, nil
	}
	return nil, fmt.Errorf("unknown operator %q", op)
}

func (c *evalCtx) str(n node, what string) (string, error) {
	v, err := c.eval(n)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("%s must be a non-empty name", what)
	}
	return s, nil
}

// canon resolves a collection name or id to its current name.
func (c *evalCtx) canon(name string) string {
	if c.app != nil {
		if col, err := c.app.FindCachedCollectionByNameOrId(name); err == nil {
			return col.Name
		}
	}
	return name
}

func (c *evalCtx) of(coll, method string) []reqView {
	coll = c.canon(coll)
	var out []reqView
	for _, r := range c.reqs {
		if r.Collection == coll && (method == "" || r.Method == method) {
			out = append(out, r)
		}
	}
	return out
}

func (c *evalCtx) evalCall(x callNode) (any, error) {
	switch x.name {
	case "req":
		iv, err := c.eval(x.args[0])
		if err != nil {
			return nil, err
		}
		f, ok := toNum(iv)
		if !ok || f != math.Trunc(f) || f < 0 || f >= float64(len(c.reqs)) {
			return nil, fmt.Errorf("req(%v): no such request (batch has %d)", iv, len(c.reqs))
		}
		r := c.reqs[int(f)]
		switch x.path[0] {
		case "method":
			return r.Method, nil
		case "collection":
			return r.Collection, nil
		case "id":
			return r.ID, nil
		}
		var cur any = r.Data
		for _, k := range x.path[1:] {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, nil
			}
			cur = m[k]
		}
		if len(x.path) == 1 {
			return nil, errors.New("req(i).body needs a field: req(i).body.<field>")
		}
		return cur, nil

	case "count":
		coll, err := c.str(x.args[0], "count() collection")
		if err != nil {
			return nil, err
		}
		method, err := c.optMethod(x.args, 1)
		if err != nil {
			return nil, err
		}
		return float64(len(c.of(coll, method))), nil

	case "sum":
		coll, err := c.str(x.args[0], "sum() collection")
		if err != nil {
			return nil, err
		}
		field, err := c.str(x.args[1], "sum() field")
		if err != nil {
			return nil, err
		}
		method, err := c.optMethod(x.args, 2)
		if err != nil {
			return nil, err
		}
		var total float64
		for _, r := range c.of(coll, method) {
			if r.Deleted || r.Data == nil || r.Data[field] == nil {
				continue
			}
			f, ok := toNum(r.Data[field])
			if !ok {
				return nil, fmt.Errorf("sum(): %s.%s of request %d is not a number", coll, field, r.Index)
			}
			total += f
			if math.IsInf(total, 0) {
				return nil, errors.New("sum(): overflow")
			}
		}
		return total, nil

	case "all", "exists":
		coll, err := c.str(x.args[0], x.name+"() collection")
		if err != nil {
			return nil, err
		}
		field, err := c.str(x.args[1], x.name+"() field")
		if err != nil {
			return nil, err
		}
		op := "=="
		vn := x.args[2]
		if x.name == "all" {
			if op, err = c.str(x.args[2], "all() operator"); err != nil {
				return nil, err
			}
			switch op {
			case "==", "!=", "<", "<=", ">", ">=":
			default:
				return nil, fmt.Errorf("all(): unknown operator %q", op)
			}
			vn = x.args[3]
		}
		want, err := c.eval(vn)
		if err != nil {
			return nil, err
		}
		numeric := false
		if c.app != nil {
			if col, err := c.app.FindCachedCollectionByNameOrId(coll); err == nil {
				_, numeric = col.Fields.GetByName(field).(*core.NumberField)
			}
		}
		for _, r := range c.of(coll, "") {
			if r.Deleted {
				continue
			}
			var got any
			if r.Data != nil {
				got = r.Data[field]
			}
			v, err := compare(got, op, want, numeric)
			if err != nil {
				return nil, err
			}
			if x.name == "all" && !v.(bool) {
				return false, nil
			}
			if x.name == "exists" && v.(bool) {
				return true, nil
			}
		}
		return x.name == "all", nil

	case "stock":
		return c.stock(x)
	}
	return nil, fmt.Errorf("unknown function %q", x.name)
}

func (c *evalCtx) optMethod(args []node, i int) (string, error) {
	if len(args) <= i {
		return "", nil
	}
	m, err := c.str(args[i], "method")
	if err != nil {
		return "", err
	}
	m = strings.ToUpper(m)
	switch m {
	case "POST", "PATCH", "DELETE", "PUT":
		return m, nil
	}
	return "", fmt.Errorf("unknown method %q", m)
}

// stock(items, id_field, qty_field, stock_collection, stock_field) returns the
// smallest slack over the ids named by the batch items: the CURRENT value of
// stock_field (read through the transaction app) minus the quantity the batch
// requests for that id. `stock(...) >= 0` therefore means every item can be
// served. It returns 0 when the batch has no such items.
func (c *evalCtx) stock(x callNode) (any, error) {
	names := make([]string, 5)
	for i := range names {
		s, err := c.str(x.args[i], "stock() argument "+strconv.Itoa(i+1))
		if err != nil {
			return nil, err
		}
		names[i] = s
	}
	items, idField, qtyField, stockColl, stockField := names[0], names[1], names[2], names[3], names[4]
	want := map[string]float64{}
	var order []string
	for _, r := range c.of(items, "") {
		if r.Deleted || r.Data == nil {
			continue
		}
		id := cast.ToString(r.Data[idField])
		if id == "" {
			return nil, fmt.Errorf("stock(): request %d has no %s", r.Index, idField)
		}
		q, ok := toNum(r.Data[qtyField])
		if !ok {
			return nil, fmt.Errorf("stock(): request %d has no numeric %s", r.Index, qtyField)
		}
		if _, seen := want[id]; !seen {
			order = append(order, id)
		}
		want[id] += q
	}
	if len(order) == 0 {
		return 0.0, nil
	}
	if c.app == nil {
		return nil, errors.New("stock() needs a database")
	}
	min := math.Inf(1)
	for _, id := range order {
		rec, err := c.app.FindRecordById(stockColl, id)
		if err != nil {
			return nil, fmt.Errorf("stock(): %s %q not found", stockColl, id)
		}
		if rec.Collection().Fields.GetByName(stockField) == nil {
			return nil, fmt.Errorf("stock(): %s has no field %q", stockColl, stockField)
		}
		if slack := rec.GetFloat(stockField) - want[id]; slack < min {
			min = slack
		}
	}
	return min, nil
}
