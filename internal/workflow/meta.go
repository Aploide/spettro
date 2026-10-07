package workflow

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/token"
)

// Meta is the header every workflow script must declare:
//
//	export const meta = { name, description, phases: [{title, detail}], params }
//
// It is read before a single agent runs, so hosts can show what a script is
// about to do (and ask for approval) without executing it. That is only sound
// if the header cannot itself run code, hence the pure-literal rule enforced
// by ParseMeta.
type Meta struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	WhenToUse   string      `json:"whenToUse,omitempty"`
	Phases      []PhaseMeta `json:"phases,omitempty"`
	Model       string      `json:"model,omitempty"`
	// Params are the template parameters the script declares, in
	// declaration order. They are what turns a saved workflow into a
	// template: the task-specific inputs are named, typed and defaulted, and
	// the engine checks them before a single agent runs.
	Params []ParamMeta `json:"params,omitempty"`
}

// ParamMeta describes one declared template parameter:
//
//	params: { base: {type: 'string', description: 'branch to diff against', default: 'main'},
//	          focus: 'what to look at' }   // shorthand: an optional string
type ParamMeta struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
}

// ParamTypes are the values meta.params[].type may take. "any" (the default)
// skips the type check.
var ParamTypes = []string{"string", "number", "boolean", "array", "object", "any"}

// PhaseMeta describes one declared phase of a workflow.
type PhaseMeta struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Model  string `json:"model,omitempty"`
}

const metaDecl = "export const meta"

// stripMetaExport rewrites the `export const meta = …` header into a plain
// declaration. Scripts are run as a function body, not as an ES module, so the
// export keyword would be a syntax error; nothing else about the declaration
// changes.
func stripMetaExport(script string) string {
	idx := indexMetaDecl(script)
	if idx < 0 {
		return script
	}
	return script[:idx] + "const meta" + script[idx+len(metaDecl):]
}

// indexMetaDecl finds the meta declaration at the start of a line (ignoring
// indentation), so the token cannot be matched inside a string or comment that
// happens to quote it.
func indexMetaDecl(script string) int {
	for offset := 0; ; {
		i := strings.Index(script[offset:], metaDecl)
		if i < 0 {
			return -1
		}
		abs := offset + i
		if lineStartIsBlank(script, abs) {
			return abs
		}
		offset = abs + len(metaDecl)
	}
}

func lineStartIsBlank(s string, idx int) bool {
	for i := idx - 1; i >= 0; i-- {
		switch s[i] {
		case '\n':
			return true
		case ' ', '\t', '\r':
			continue
		default:
			return false
		}
	}
	return true
}

// metaEvalTimeout bounds evaluating a header. A pure literal evaluates in
// microseconds; anything slower is code, and ParseMeta runs on paths where a
// hang is unacceptable — listing saved workflows, the ACP workflow list, the
// tool call itself.
const metaEvalTimeout = time.Second

var errMetaTimeout = errors.New("evaluating it took longer than 1s — it is code, not a literal")

// ParseMeta extracts and evaluates the meta header without running the script.
//
// The header is first parsed and checked node by node (checkLiteral): only
// literals get through, so a header that calls a function, reads a variable,
// or carries a getter or a regular expression fails here rather than silently
// doing work before the user has seen what the workflow is.
//
// The check is what actually holds the line. Evaluating the literal afterwards
// happens in a throwaway runtime stripped of every global, under a watchdog,
// but neither is a defence on its own: a literal's own prototype chain still
// leads somewhere ('x'.constructor is String), and the watchdog's interrupt
// cannot stop a regular expression backtracking inside native Go code — a
// header like /^(a+)+(?=b)/.test('aaaa…c') ran for as long as it liked. They
// stay as a backstop.
func ParseMeta(script string) (m Meta, err error) {
	decl := indexMetaDecl(script)
	if decl < 0 {
		return Meta{}, fmt.Errorf("script must begin with `export const meta = {...}` declaring name, description and phases")
	}
	eq := strings.IndexByte(script[decl:], '=')
	if eq < 0 {
		return Meta{}, fmt.Errorf("malformed meta declaration: no `=` after `export const meta`")
	}
	body := script[decl+eq+1:]
	literal, err := objectLiteral(body)
	if err != nil {
		return Meta{}, fmt.Errorf("malformed meta declaration: %w", err)
	}
	if err := checkLiteral(literal); err != nil {
		return Meta{}, fmt.Errorf("meta must be a pure object literal (no variables, calls, or spreads): %w", err)
	}

	vm := goja.New()
	// A pure literal needs no globals at all. Emptying the global object is
	// what turns "must be a literal" from documentation into a rule: any
	// identifier or call in the header now throws a ReferenceError. The
	// builtins are non-enumerable, so Keys() would list none of them; the own
	// property names include them. NaN, Infinity and undefined are
	// non-configurable and survive, which is harmless: they are values.
	global := vm.GlobalObject()
	for _, key := range global.GetOwnPropertyNames() {
		_ = global.Delete(key)
	}

	timer := time.AfterFunc(metaEvalTimeout, func() { vm.Interrupt(errMetaTimeout) })
	defer timer.Stop()
	// Reading the result back can run JS too (a getter in the literal), and an
	// interrupt that lands there arrives as a panic rather than an error.
	defer func() {
		if rec := recover(); rec != nil {
			if _, ok := rec.(*goja.InterruptedError); ok {
				m, err = Meta{}, fmt.Errorf("meta must be a pure object literal: %w", errMetaTimeout)
				return
			}
			m, err = Meta{}, fmt.Errorf("meta must be a pure object literal: %v", rec)
		}
	}()

	val, err := vm.RunString("(" + literal + ")")
	if err != nil {
		var interrupted *goja.InterruptedError
		if errors.As(err, &interrupted) {
			return Meta{}, fmt.Errorf("meta must be a pure object literal: %w", errMetaTimeout)
		}
		return Meta{}, fmt.Errorf("meta must be a pure object literal (no variables, calls, or spreads): %w", err)
	}
	obj, ok := val.Export().(map[string]any)
	if !ok {
		return Meta{}, fmt.Errorf("meta must be an object literal")
	}
	m = Meta{
		Name:        stringField(obj, "name"),
		Description: stringField(obj, "description"),
		WhenToUse:   stringField(obj, "whenToUse"),
		Model:       stringField(obj, "model"),
	}
	if raw, ok := obj["phases"].([]any); ok {
		for _, entry := range raw {
			p, ok := entry.(map[string]any)
			if !ok {
				return Meta{}, fmt.Errorf("meta.phases entries must be objects with a title")
			}
			m.Phases = append(m.Phases, PhaseMeta{
				Title:  stringField(p, "title"),
				Detail: stringField(p, "detail"),
				Model:  stringField(p, "model"),
			})
		}
	}
	if m.Name == "" {
		return Meta{}, fmt.Errorf("meta.name is required")
	}
	if m.Description == "" {
		return Meta{}, fmt.Errorf("meta.description is required")
	}
	for i, p := range m.Phases {
		if p.Title == "" {
			return Meta{}, fmt.Errorf("meta.phases[%d].title is required", i)
		}
	}
	params, err := parseParams(val.ToObject(vm).Get("params"))
	if err != nil {
		return Meta{}, err
	}
	m.Params = params
	return m, nil
}

// parseParams reads meta.params off the evaluated literal. It works on the
// goja object rather than the exported Go map because declaration order is
// part of the contract — it is the order hosts list the params in, and the
// exported map has none.
func parseParams(v goja.Value) ([]ParamMeta, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	obj, ok := v.(*goja.Object)
	if !ok || obj.ClassName() == "Array" {
		return nil, fmt.Errorf("meta.params must be an object: {name: {type, description, required, default}} or {name: 'description'}")
	}
	var params []ParamMeta
	for _, name := range obj.Keys() {
		p, err := parseParam(name, obj.Get(name))
		if err != nil {
			return nil, err
		}
		params = append(params, p)
	}
	return params, nil
}

func parseParam(name string, v goja.Value) (ParamMeta, error) {
	p := ParamMeta{Name: name, Type: "any"}
	if strings.TrimSpace(name) == "" {
		return p, fmt.Errorf("meta.params: a param needs a name")
	}
	switch raw := v.Export().(type) {
	case string:
		// The shorthand {name: 'description'} is an optional string: the
		// commonest param by far is "the thing to work on".
		p.Type = "string"
		p.Description = strings.TrimSpace(raw)
		return p, nil
	case map[string]any:
		if t, present := raw["type"]; present {
			ts, _ := t.(string)
			ts = strings.ToLower(strings.TrimSpace(ts))
			if !validParamType(ts) {
				return p, fmt.Errorf("meta.params.%s.type must be one of %s, got %v", name, strings.Join(ParamTypes, "|"), t)
			}
			p.Type = ts
		}
		p.Description = stringField(raw, "description")
		if r, present := raw["required"]; present && r != nil {
			b, ok := r.(bool)
			if !ok {
				return p, fmt.Errorf("meta.params.%s.required must be true or false", name)
			}
			p.Required = b
		}
		if d, present := raw["default"]; present && d != nil {
			if !paramTypeMatches(p.Type, d) {
				return p, fmt.Errorf("meta.params.%s.default must be a %s, got %s", name, p.Type, jsTypeName(d))
			}
			p.Default = d
		}
		return p, nil
	default:
		return p, fmt.Errorf("meta.params.%s must be a description string or {type, description, required, default}", name)
	}
}

func validParamType(t string) bool {
	for _, known := range ParamTypes {
		if t == known {
			return true
		}
	}
	return false
}

// paramTypeMatches checks a value against a declared param type. Values come
// from two places — exported goja values (defaults) and JSON-decoded tool
// arguments — so numbers may be any Go numeric type and arrays any slice.
func paramTypeMatches(want string, v any) bool {
	if want == "any" || want == "" {
		return true
	}
	return jsTypeName(v) == want
}

// jsTypeName names a Go value by the JS/JSON type a script would see.
func jsTypeName(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	switch reflect.TypeOf(v).Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	case reflect.String:
		// json.Number and other named string types.
		if _, ok := v.(interface{ Float64() (float64, error) }); ok {
			return "number"
		}
		return "string"
	}
	return reflect.TypeOf(v).Kind().String()
}

// applyParams checks args against the script's declared params and fills in
// defaults, before any agent runs: a template run with a missing input should
// fail at the door with the names of what is missing, not halfway through with
// "cannot read property of undefined".
//
// The caller's args are never mutated. A script that declares no params gets
// its args back untouched, so static workflows behave exactly as before.
func applyParams(meta Meta, args any, depth int) (any, error) {
	if len(meta.Params) == 0 {
		return args, nil
	}
	where := "the tool call's args"
	if depth > 0 {
		where = "the workflow() call's args"
	}
	values := map[string]any{}
	switch a := args.(type) {
	case nil:
	case map[string]any:
		for k, v := range a {
			values[k] = v
		}
	default:
		// A template with one obvious input should accept it bare:
		// /workflows run review "the auth module" is not worth an error.
		target := soleParam(meta.Params)
		if target == "" {
			return nil, fmt.Errorf("workflow %q: args must be an object with the declared params (%s) — pass them in %s", meta.Name, paramNames(meta.Params), where)
		}
		values[target] = args
	}
	var missing, wrong []string
	for _, p := range meta.Params {
		v, present := values[p.Name]
		if !present || v == nil {
			if p.Default != nil {
				values[p.Name] = p.Default
				continue
			}
			if p.Required {
				missing = append(missing, p.Name)
			}
			continue
		}
		if !paramTypeMatches(p.Type, v) {
			wrong = append(wrong, fmt.Sprintf("%s (want %s, got %s)", p.Name, p.Type, jsTypeName(v)))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("workflow %q: missing required param(s): %s — pass them in %s", meta.Name, strings.Join(missing, ", "), where)
	}
	if len(wrong) > 0 {
		return nil, fmt.Errorf("workflow %q: param(s) of the wrong type: %s — fix them in %s", meta.Name, strings.Join(wrong, ", "), where)
	}
	return values, nil
}

// soleParam names the param a bare (non-object) args value binds to: the only
// required param, or failing that the only param. "" when it is ambiguous.
func soleParam(params []ParamMeta) string {
	var required []string
	for _, p := range params {
		if p.Required {
			required = append(required, p.Name)
		}
	}
	switch {
	case len(required) == 1:
		return required[0]
	case len(required) == 0 && len(params) == 1:
		return params[0].Name
	}
	return ""
}

func paramNames(params []ParamMeta) string {
	names := make([]string, len(params))
	for i, p := range params {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// checkLiteral parses a header's object literal and walks it, accepting only
// what a static description needs: object and array literals with plain keys,
// strings, numbers, booleans, null, the value identifiers undefined, NaN and
// Infinity, template literals without substitutions, a unary sign on a
// number, and + between accepted values (long descriptions are written as
// concatenated strings).
//
// Everything else is code — calls, identifiers, getters and methods, computed
// keys, spreads, regular expressions, functions — and is rejected before
// anything is evaluated, which is what lets ParseMeta run on untrusted files
// (a cloned repository's saved workflows) without a hostile header being able
// to hang it.
func checkLiteral(literal string) error {
	prog, err := parser.ParseFile(nil, "", "("+literal+")", 0)
	if err != nil {
		return err
	}
	if len(prog.Body) != 1 {
		return fmt.Errorf("expected a single object literal")
	}
	stmt, ok := prog.Body[0].(*ast.ExpressionStatement)
	if !ok {
		return fmt.Errorf("expected a single object literal")
	}
	return checkLiteralNode(stmt.Expression, 0)
}

// maxLiteralDepth bounds nesting so a pathological header cannot exhaust the
// stack of the walk itself.
const maxLiteralDepth = 64

// maxConcatTerms bounds one chain of concatenated strings in a header: far
// past any real description, short of a header built to make the check slow.
const maxConcatTerms = 4096

func checkLiteralNode(e ast.Expression, depth int) error {
	if depth > maxLiteralDepth {
		return fmt.Errorf("nested more than %d levels deep", maxLiteralDepth)
	}
	switch n := e.(type) {
	case *ast.StringLiteral, *ast.NumberLiteral, *ast.BooleanLiteral, *ast.NullLiteral:
		return nil
	case *ast.Identifier:
		// The three non-configurable global values: names, but not variables.
		switch n.Name {
		case "undefined", "NaN", "Infinity":
			return nil
		}
		return fmt.Errorf("%q is a variable", n.Name)
	case *ast.TemplateLiteral:
		if n.Tag != nil || len(n.Expressions) > 0 {
			return fmt.Errorf("a template literal with ${…} substitutions is code")
		}
		return nil
	case *ast.UnaryExpression:
		if n.Postfix || (n.Operator != token.MINUS && n.Operator != token.PLUS) {
			return fmt.Errorf("operator %s is code", n.Operator)
		}
		switch operand := n.Operand.(type) {
		case *ast.NumberLiteral:
			return nil
		case *ast.Identifier:
			if operand.Name == "Infinity" {
				return nil
			}
		}
		return fmt.Errorf("a sign applies only to a number")
	case *ast.BinaryExpression:
		// A long description is a chain of concatenated strings, which
		// parses left-nested: 'a' + 'b' + 'c' is ('a' + 'b') + 'c'. Walking
		// the chain iteratively keeps its length from counting as nesting —
		// recursing put a 65-piece description past the depth cap with an
		// error about nesting its author never wrote. The term cap bounds
		// the walk instead.
		var cur ast.Expression = n
		for terms := 0; ; terms++ {
			if terms > maxConcatTerms {
				return fmt.Errorf("more than %d concatenated pieces", maxConcatTerms)
			}
			bin, ok := cur.(*ast.BinaryExpression)
			if !ok {
				return checkLiteralNode(cur, depth)
			}
			if bin.Operator != token.PLUS {
				return fmt.Errorf("operator %s is code", bin.Operator)
			}
			if err := checkLiteralNode(bin.Right, depth); err != nil {
				return err
			}
			cur = bin.Left
		}
	case *ast.ArrayLiteral:
		for _, v := range n.Value {
			if v == nil { // a hole: [1, , 2]
				continue
			}
			if err := checkLiteralNode(v, depth+1); err != nil {
				return err
			}
		}
		return nil
	case *ast.ObjectLiteral:
		for _, prop := range n.Value {
			keyed, ok := prop.(*ast.PropertyKeyed)
			if !ok {
				// A shorthand {name} reads a variable; a spread runs code.
				return fmt.Errorf("only `key: value` properties are allowed")
			}
			if keyed.Kind != ast.PropertyKindValue {
				return fmt.Errorf("a %s property is code", keyed.Kind)
			}
			if keyed.Computed {
				return fmt.Errorf("a computed [key] is code")
			}
			switch keyed.Key.(type) {
			case *ast.StringLiteral, *ast.NumberLiteral:
			default:
				return fmt.Errorf("a property key must be a name, string or number")
			}
			if err := checkLiteralNode(keyed.Value, depth+1); err != nil {
				return err
			}
		}
		return nil
	case *ast.RegExpLiteral:
		return fmt.Errorf("a regular expression is code")
	case *ast.CallExpression, *ast.NewExpression:
		return fmt.Errorf("a call is code")
	case *ast.FunctionLiteral, *ast.ArrowFunctionLiteral, *ast.ClassLiteral:
		return fmt.Errorf("a function is code")
	}
	return fmt.Errorf("a %s is not a literal", strings.TrimPrefix(fmt.Sprintf("%T", e), "*ast."))
}

// objectLiteral returns the `{...}` starting the given text, brace-matched
// while skipping strings, template literals and comments so a `}` inside a
// description does not truncate the header.
func objectLiteral(s string) (string, error) {
	start := -1
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			start = i
		}
		break
	}
	if start < 0 {
		return "", fmt.Errorf("expected an object literal after `=`")
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch c := s[i]; c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		case '\'', '"', '`':
			end, err := skipString(s, i, c)
			if err != nil {
				return "", err
			}
			i = end
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				if nl := strings.IndexByte(s[i:], '\n'); nl < 0 {
					return "", fmt.Errorf("unterminated meta object")
				} else {
					i += nl
				}
			} else if i+1 < len(s) && s[i+1] == '*' {
				end := strings.Index(s[i+2:], "*/")
				if end < 0 {
					return "", fmt.Errorf("unterminated comment in meta object")
				}
				i += 2 + end + 1
			}
		}
	}
	return "", fmt.Errorf("unterminated meta object")
}

// skipString returns the index of the closing quote for the string starting at
// open (which holds the quote character).
func skipString(s string, open int, quote byte) (int, error) {
	for i := open + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case quote:
			return i, nil
		}
	}
	return 0, fmt.Errorf("unterminated string in meta object")
}
