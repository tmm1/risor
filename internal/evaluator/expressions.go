package evaluator

import (
	"context"

	"github.com/myzie/tamarin/internal/ast"
	"github.com/myzie/tamarin/internal/object"
	"github.com/myzie/tamarin/internal/scope"
)

func (e *Evaluator) evalExpressions(
	ctx context.Context,
	exps []ast.Expression,
	s *scope.Scope,
) []object.Object {
	values := make([]object.Object, len(exps))
	for i, exp := range exps {
		value := e.Evaluate(ctx, exp, s)
		if isError(value) {
			return []object.Object{value}
		}
		values[i] = value
	}
	return values
}