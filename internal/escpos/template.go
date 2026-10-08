package escpos

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
	"time"
	"unicode/utf8"
)

// Limits bound the work a template may cause.
type Limits struct {
	MaxTemplate int // template source bytes
	MaxOutput   int // rendered text bytes (before directives are turned into bytes)
	MaxBytes    int // resulting ESC/POS stream
	MaxData     int // nodes in the data tree
	MaxDepth    int // nesting depth of the data tree
	MaxString   int // bytes of one data string
}

// DefaultLimits are the limits used when [Options.Limits] is the zero value.
var DefaultLimits = Limits{MaxTemplate: 16 << 10, MaxOutput: 128 << 10, MaxBytes: 64 << 10, MaxData: 5000, MaxDepth: 8, MaxString: 4096}

// Options configure [Render].
type Options struct {
	Codepage Codepage
	// QRRaster makes @qr print an image (printers without native QR).
	QRRaster bool
	// Location is used by the date function (default UTC).
	Location *time.Location
	Limits   Limits
}

// ErrLimit is wrapped by every limit violation.
var ErrLimit = errors.New("escpos: limit exceeded")

// Render executes tpl with data and turns the result into ESC/POS bytes.
//
// Lines of the rendered text that start with '@' are directives: @center,
// @left, @right, @size W H, @bold [on|off], @feed N, @cut, @drawer,
// @qr [size=N] [ec=L|M|Q|H] DATA, @barcode code128|ean13 DATA, @codepage NAME.
// "@@" at the start of a line prints a literal '@'. A directive may end with
// a comment: two or more spaces (or a tab) followed by '#'. Every other line is
// printed as text. The stream starts with ESC @ and the codepage selection.
//
// The template can use pad, padl, money, date and upper. It cannot define or
// call other templates (no recursion) and has no I/O function.
func Render(tpl string, data any, o Options) ([]byte, error) {
	out, _, err := RenderInfo(tpl, data, o)
	return out, err
}

// Info reports which directives the template itself used.
type Info struct {
	HasCut    bool // the template contains an explicit @cut
	HasDrawer bool // the template contains an explicit @drawer
}

// atSentinel stands in for a '@' that starts a data string: data values are
// never directives. compile turns it back into a literal '@'.
const atSentinel = '\uE000'

// sanitizeData returns a copy of the JSON-like data with every string made
// safe: control characters (newlines included) become spaces and a leading
// '@' is replaced by atSentinel, so a value can never start a directive line
// or smuggle raw bytes into @qr / @barcode. Typed Go values (structs) pass
// through unchanged: only decoded request data is untrusted.
func sanitizeData(v any) any {
	switch x := v.(type) {
	case string:
		return sanitizeString(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = sanitizeData(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = sanitizeData(e)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, e := range x {
			out[k] = sanitizeString(e)
		}
		return out
	case []string:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = sanitizeString(e)
		}
		return out
	}
	return v
}

func sanitizeString(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			clean = false
			break
		}
	}
	if clean && (s == "" || s[0] != '@') {
		return s
	}
	r := []rune(s)
	for i, c := range r {
		if c < 0x20 || c == 0x7f || c == 0x2028 || c == 0x2029 {
			r[i] = ' '
		}
	}
	if len(r) > 0 && r[0] == '@' {
		r[0] = atSentinel
	}
	return string(r)
}

// RenderInfo is [Render] that also reports the explicit @cut / @drawer of the
// template (never of data).
func RenderInfo(tpl string, data any, o Options) ([]byte, Info, error) {
	lim := o.Limits
	if lim == (Limits{}) {
		lim = DefaultLimits
	}
	if len(tpl) > lim.MaxTemplate {
		return nil, Info{}, fmt.Errorf("%w: template is %d bytes", ErrLimit, len(tpl))
	}
	if err := checkData(reflect.ValueOf(data), lim, new(int), 0); err != nil {
		return nil, Info{}, err
	}
	data = sanitizeData(data)
	loc := o.Location
	if loc == nil {
		loc = time.UTC
	}
	t, err := template.New("p").Option("missingkey=zero").Funcs(funcMap(loc)).Parse(tpl)
	if err != nil {
		return nil, Info{}, err
	}
	if err := checkTree(t); err != nil {
		return nil, Info{}, err
	}
	w := &limitWriter{max: lim.MaxOutput}
	if err := t.Execute(w, data); err != nil {
		return nil, Info{}, err
	}
	// a missing key prints as nothing, not as "<no value>"
	text := strings.ReplaceAll(w.buf.String(), "<no value>", "")
	b, err := compile(text, o, lim)
	if err != nil {
		return nil, Info{}, err
	}
	return b.Bytes(), Info{HasCut: b.hadCut, HasDrawer: b.hadDrawer}, nil
}

func checkTree(t *template.Template) error {
	for _, tt := range t.Templates() {
		if tt.Tree == nil {
			continue
		}
		if tt.Name() != "p" {
			return errors.New("escpos: define/block is not allowed")
		}
		var walk func(n parse.Node) error
		walk = func(n parse.Node) error {
			switch n := n.(type) {
			case *parse.TemplateNode:
				return errors.New("escpos: template calls are not allowed")
			case *parse.ActionNode:
				return checkPipe(n.Pipe)
			case *parse.ListNode:
				if n == nil {
					return nil
				}
				for _, c := range n.Nodes {
					if err := walk(c); err != nil {
						return err
					}
				}
			case *parse.IfNode:
				return walkBranch(walk, &n.BranchNode)
			case *parse.RangeNode:
				return walkBranch(walk, &n.BranchNode)
			case *parse.WithNode:
				return walkBranch(walk, &n.BranchNode)
			}
			return nil
		}
		if err := walk(tt.Tree.Root); err != nil {
			return err
		}
	}
	return nil
}

// checkPipe rejects the builtin "call" (the plan allows no function values).
func checkPipe(p *parse.PipeNode) error {
	if p == nil {
		return nil
	}
	for _, c := range p.Cmds {
		for _, a := range c.Args {
			switch a := a.(type) {
			case *parse.IdentifierNode:
				if a.Ident == "call" {
					return errors.New("escpos: call is not allowed")
				}
			case *parse.PipeNode:
				if err := checkPipe(a); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func walkBranch(walk func(parse.Node) error, b *parse.BranchNode) error {
	if err := checkPipe(b.Pipe); err != nil {
		return err
	}
	if err := walk(b.List); err != nil {
		return err
	}
	if b.ElseList != nil {
		return walk(b.ElseList)
	}
	return nil
}

type limitWriter struct {
	buf bytes.Buffer
	max int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		return 0, fmt.Errorf("%w: output over %d bytes", ErrLimit, w.max)
	}
	return w.buf.Write(p)
}

func checkData(v reflect.Value, lim Limits, n *int, depth int) error {
	if *n++; *n > lim.MaxData {
		return fmt.Errorf("%w: data has more than %d values", ErrLimit, lim.MaxData)
	}
	if depth > lim.MaxDepth {
		return fmt.Errorf("%w: data nested deeper than %d", ErrLimit, lim.MaxDepth)
	}
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.String:
		if v.Len() > lim.MaxString {
			return fmt.Errorf("%w: a string is longer than %d bytes", ErrLimit, lim.MaxString)
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			if err := checkData(it.Value(), lim, n, depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := checkData(v.Index(i), lim, n, depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				if err := checkData(v.Field(i), lim, n, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ---- functions ----

func funcMap(loc *time.Location) template.FuncMap {
	return template.FuncMap{
		"upper": func(v any) string { return strings.ToUpper(fmt.Sprint(v)) },
		"pad":   func(v any, n int) string { return padRight(fmt.Sprint(v), n) },
		"padl":  func(v any, n int) string { return padLeft(fmt.Sprint(v), n) },
		"money": money,
		"date":  func(v any, layout string) (string, error) { return formatDate(v, layout, loc) },
	}
}

func padRight(s string, n int) string {
	n = clamp(n, 0, 200)
	c := utf8.RuneCountInString(s)
	if c >= n {
		return string([]rune(s)[:n])
	}
	return s + strings.Repeat(" ", n-c)
}

func padLeft(s string, n int) string {
	n = clamp(n, 0, 200)
	c := utf8.RuneCountInString(s)
	if c >= n {
		r := []rune(s)
		return string(r[c-n:])
	}
	return strings.Repeat(" ", n-c) + s
}

// money formats a whole number of currency units with '.' as thousands
// separator (15000 -> "15.000"); decimals are rounded.
func money(v any) (string, error) {
	var f float64
	switch x := v.(type) {
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case int32:
		f = float64(x)
	case uint:
		f = float64(x)
	case uint64:
		f = float64(x)
	case float64:
		f = x
	case float32:
		f = float64(x)
	case string:
		p, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return "", fmt.Errorf("money: %q is not a number", x)
		}
		f = p
	default:
		return "", fmt.Errorf("money: unsupported %T", v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1e15 {
		return "", errors.New("money: out of range")
	}
	n := int64(math.Round(math.Abs(f)))
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	if f < 0 && n != 0 {
		b.WriteByte('-')
	}
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	return b.String(), nil
}

// formatDate accepts time.Time, an RFC 3339 / "2006-01-02 15:04:05" string, or
// Unix seconds.
func formatDate(v any, layout string, loc *time.Location) (string, error) {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case int64:
		t = time.Unix(x, 0)
	case int:
		t = time.Unix(int64(x), 0)
	case float64:
		t = time.Unix(int64(x), 0)
	case string:
		var err error
		for _, l := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999", "2006-01-02 15:04:05", "2006-01-02"} {
			if t, err = time.Parse(l, x); err == nil {
				break
			}
		}
		if err != nil {
			return "", fmt.Errorf("date: cannot parse %q", x)
		}
	default:
		return "", fmt.Errorf("date: unsupported %T", v)
	}
	if len(layout) > 64 {
		return "", errors.New("date: layout too long")
	}
	return t.In(loc).Format(layout), nil
}

// ---- directives ----

func compile(text string, o Options, lim Limits) (*Builder, error) {
	b := New(o.Codepage)
	b.QRRaster = o.QRRaster
	b.Init()
	text = strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		return b, nil
	}
	for i, line := range strings.Split(text, "\n") {
		if i > 0 && b.Len() > lim.MaxBytes {
			return nil, fmt.Errorf("%w: stream over %d bytes", ErrLimit, lim.MaxBytes)
		}
		if strings.HasPrefix(line, "@@") {
			b.Line(unsentinel(line[1:]))
			continue
		}
		if !strings.HasPrefix(line, "@") {
			b.Line(unsentinel(line))
			continue
		}
		if err := directive(b, strings.TrimSpace(stripComment(unsentinel(line[1:])))); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
	}
	if b.Len() > lim.MaxBytes {
		return nil, fmt.Errorf("%w: stream over %d bytes", ErrLimit, lim.MaxBytes)
	}
	return b, nil
}

func unsentinel(s string) string {
	if strings.ContainsRune(s, atSentinel) {
		return strings.ReplaceAll(s, string(atSentinel), "@")
	}
	return s
}

func stripComment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && i > 0 && (s[i-1] == '\t' || (s[i-1] == ' ' && i > 1 && s[i-2] == ' ')) {
			return s[:i]
		}
	}
	return s
}

func directive(b *Builder, d string) error {
	name, arg, _ := strings.Cut(d, " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "center":
		b.Align(AlignCenter)
	case "left":
		b.Align(AlignLeft)
	case "right":
		b.Align(AlignRight)
	case "size":
		f := strings.Fields(arg)
		if len(f) != 2 {
			return errors.New("@size needs width and height")
		}
		w, e1 := strconv.Atoi(f[0])
		h, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil || w < 1 || w > 8 || h < 1 || h > 8 {
			return errors.New("@size values must be 1 to 8")
		}
		b.Size(w, h)
	case "bold":
		switch arg {
		case "", "on":
			b.Bold(true)
		case "off":
			b.Bold(false)
		default:
			return errors.New("@bold takes on or off")
		}
	case "feed":
		n := 1
		if arg != "" {
			var err error
			if n, err = strconv.Atoi(arg); err != nil || n < 0 || n > 255 {
				return errors.New("@feed takes 0 to 255")
			}
		}
		b.Feed(n)
	case "cut":
		b.Cut()
	case "drawer":
		b.Drawer()
	case "codepage":
		cp, ok := ParseCodepage(strings.ToLower(arg))
		if !ok {
			return fmt.Errorf("unknown codepage %q", arg)
		}
		b.Codepage(cp)
	case "qr":
		return qrDirective(b, arg)
	case "barcode":
		kind, data, _ := strings.Cut(arg, " ")
		return b.Barcode(strings.ToLower(kind), strings.TrimSpace(data))
	default:
		return fmt.Errorf("unknown directive @%s", name)
	}
	return nil
}

// qrDirective parses "[size=N] [ec=L|M|Q|H] DATA..."; DATA is the rest of the
// line and may contain spaces.
func qrDirective(b *Builder, arg string) error {
	size, level := 4, QRMedium
	for {
		w, rest, _ := strings.Cut(arg, " ")
		switch {
		case strings.HasPrefix(w, "size="):
			n, err := strconv.Atoi(w[5:])
			if err != nil || n < 1 || n > 16 {
				return errors.New("@qr size must be 1 to 16")
			}
			size = n
		case strings.HasPrefix(w, "ec="):
			switch strings.ToUpper(w[3:]) {
			case "L":
				level = QRLow
			case "M":
				level = QRMedium
			case "Q":
				level = QRQuartile
			case "H":
				level = QRHigh
			default:
				return errors.New("@qr ec must be L, M, Q or H")
			}
		default:
			if arg == "" {
				return errors.New("@qr needs data")
			}
			return b.QR(arg, size, level)
		}
		arg = strings.TrimSpace(rest)
	}
}
