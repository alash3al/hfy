package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // IANA zones even where the system has none (scratch, distroless)

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"

	"github.com/alash3al/hfy/internal/mcp"
)

func fail(msg string) mcp.Result { return mcp.Result{Content: msg, IsError: true} }

func now(_ context.Context, args json.RawMessage) (mcp.Result, error) {
	var in struct {
		Timezone string `json:"timezone"`
	}
	if len(args) > 0 && json.Unmarshal(args, &in) != nil {
		return fail(`arguments must be {"timezone": "<IANA zone>"}`), nil
	}
	loc := time.UTC
	if in.Timezone != "" {
		var err error
		if loc, err = time.LoadLocation(in.Timezone); err != nil || in.Timezone == "Local" {
			return fail(fmt.Sprintf("unknown time zone %q: use an IANA name such as \"Europe/Berlin\"", in.Timezone)), nil
		}
	}
	t := time.Now().In(loc)
	b, _ := json.Marshal(map[string]string{
		"utc": t.UTC().Format(time.RFC3339), "timezone": loc.String(), "local": t.Format(time.RFC3339),
		"weekday": t.Weekday().String(), "utc_offset": t.Format("-07:00"),
	})
	return mcp.Result{Content: string(b)}, nil
}

// mathBuiltins are the only functions math_calc offers; operators are syntax.
var mathBuiltins = []string{"abs", "ceil", "floor", "round", "int", "float", "min", "max", "sum", "mean", "median"}

const maxExpression = 2000

func mathCalc(_ context.Context, args json.RawMessage) (mcp.Result, error) {
	var in struct {
		Expression string `json:"expression"`
	}
	if json.Unmarshal(args, &in) != nil || strings.TrimSpace(in.Expression) == "" {
		return fail("expression is required"), nil
	}
	if len(in.Expression) > maxExpression {
		return fail("expression is too long"), nil
	}
	opts := []expr.Option{expr.DisableAllBuiltins(), expr.MaxNodes(500), expr.Patch(&floats{made: map[*ast.FloatNode]int{}})}
	for _, name := range mathBuiltins {
		opts = append(opts, expr.EnableBuiltin(name))
	}
	prog, err := expr.Compile(in.Expression, opts...)
	if err != nil {
		return fail(err.Error()), nil
	}
	out, err := expr.Run(prog, nil)
	if err != nil {
		return fail(err.Error()), nil
	}
	text, err := number(out)
	if err != nil {
		return fail(err.Error()), nil
	}
	return mcp.Result{Content: text}, nil
}

// number formats a result. Floats get 15 significant digits, which float64
// always holds, so binary noise never shows (0.1 + 0.2 is "0.3").
func number(v any) (string, error) {
	switch x := v.(type) {
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return "", fmt.Errorf("no finite result (division by zero or overflow)")
		}
		return strconv.FormatFloat(x, 'g', 15, 64), nil
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			s, err := number(e)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	}
	return fmt.Sprint(v), nil
}

// floats makes every integer literal a float64, so arithmetic cannot wrap
// around silently (expr's ints do: 9999999999 * 9999999999 would come out
// as 7766279611452241921). Operands that must be integers (of %, of a
// range, an index) stay integers. Children are visited before parents.
type floats struct{ made map[*ast.FloatNode]int }

func (f *floats) Visit(node *ast.Node) {
	switch n := (*node).(type) {
	case *ast.IntegerNode:
		fl := &ast.FloatNode{Value: float64(n.Value)}
		f.made[fl] = n.Value
		ast.Patch(node, fl)
	case *ast.BinaryNode:
		if n.Operator == "%" || n.Operator == ".." {
			f.back(&n.Left)
			f.back(&n.Right)
		}
	case *ast.MemberNode:
		f.back(&n.Property)
	}
}

// back turns a literal that floats made (possibly negated) into an integer again.
func (f *floats) back(node *ast.Node) {
	switch n := (*node).(type) {
	case *ast.FloatNode:
		if v, ok := f.made[n]; ok {
			ast.Patch(node, &ast.IntegerNode{Value: v})
		}
	case *ast.UnaryNode:
		f.back(&n.Node)
	}
}
