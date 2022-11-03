package evaluator

import (
	"context"
	"fmt"

	"github.com/myzie/tamarin/internal/ast"
	"github.com/myzie/tamarin/internal/object"
	"github.com/myzie/tamarin/internal/scope"
)

func (e *Evaluator) evalFunctionLiteral(
	ctx context.Context,
	node *ast.FunctionLiteral,
	s *scope.Scope,
) object.Object {
	return &object.Function{
		Parameters: node.Parameters,
		Body:       node.Body,
		Defaults:   node.Defaults,
		Scope:      s,
	}
}

func (e *Evaluator) evalFunctionDefinition(
	ctx context.Context,
	node *ast.FunctionDefineLiteral,
	s *scope.Scope,
) object.Object {
	fn := &object.Function{
		Parameters: node.Parameters,
		Body:       node.Body,
		Defaults:   node.Defaults,
		Scope:      s,
	}
	if err := s.Declare(node.TokenLiteral(), fn, true); err != nil {
		return newError(err.Error())
	}
	return object.NULL
}

func (e *Evaluator) applyFunction(
	ctx context.Context,
	s *scope.Scope,
	fn object.Object,
	args []object.Object,
) object.Object {
	switch fn := fn.(type) {
	case *object.Function:
		// Use the function's scope, not the current execution scope! This is
		// what enables closures to work as expected!
		nestedScope, err := e.newFunctionScope(ctx, fn.Scope.(*scope.Scope), fn, args)
		if err != nil {
			return newError(err.Error())
		}
		return e.upwrapReturnValue(e.Evaluate(ctx, fn.Body, nestedScope))
	case *object.Builtin:
		return fn.Fn(ctx, args...)
	default:
		return newError("type error: %s is not callable", fn.Type())
	}
}

func (e *Evaluator) newFunctionScope(
	ctx context.Context,
	s *scope.Scope,
	fn *object.Function,
	args []object.Object,
) (*scope.Scope, error) {
	nestedScope := s.NewChild(scope.Opts{Name: "function"})
	for key, val := range fn.Defaults {
		evaluatedValue := e.Evaluate(ctx, val, s)
		if isError(evaluatedValue) {
			return nil, fmt.Errorf("failed to evaluate parameter: %s", key)
		}
		if err := nestedScope.Declare(key, evaluatedValue, false); err != nil {
			return nil, err
		}
	}
	for paramIdx, param := range fn.Parameters {
		if paramIdx < len(args) {
			if _, ok := nestedScope.Get(param.Value); ok {
				if err := nestedScope.Update(param.Value, args[paramIdx]); err != nil {
					return nil, err
				}
			} else {
				if err := nestedScope.Declare(param.Value, args[paramIdx], false); err != nil {
					return nil, err
				}
			}
		} else {
			break
		}
	}
	return nestedScope, nil
}